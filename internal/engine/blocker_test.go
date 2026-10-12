package engine

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"perco-meal/internal/config"
	"perco-meal/internal/meals"
	"perco-meal/internal/store"
)

type fakePERCo struct {
	blocked map[int64]bool
	active  map[int64]bool
	fail    error // returned by every call while set
	sets    []string
}

func (f *fakePERCo) StaffBlockState(_ context.Context, id int64) (bool, bool, error) {
	if f.fail != nil {
		return false, false, f.fail
	}
	act, ok := f.active[id]
	return f.blocked[id], !ok || act, nil
}

func (f *fakePERCo) SetBlocked(_ context.Context, id int64, v bool) error {
	if f.fail != nil {
		return f.fail
	}
	f.blocked[id] = v
	if v {
		f.sets = append(f.sets, "block")
	} else {
		f.sets = append(f.sets, "unblock")
	}
	return nil
}

var tz = time.FixedZone("UTC+5", 5*3600)

// Windows as configured in the customer's PERCo (schedules 12, 13, 14).
var live = []meals.Meal{
	{Key: "b", Name: "Завтрак", TemplateID: 3, Intervals: []meals.Interval{{Begin: 25500, End: 29700}}},
	{Key: "l", Name: "Обед", TemplateID: 4, Intervals: []meals.Interval{{Begin: 43200, End: 48600}}},
	{Key: "d", Name: "Ужин", TemplateID: 5, Intervals: []meals.Interval{{Begin: 66600, End: 70200}}},
}

type rig struct {
	b    *Blocker
	api  *fakePERCo
	st   *store.Store
	now  time.Time
	ok   bool
	next int64
}

func newRig(t *testing.T, mode string) *rig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{}
	cfg.Blocking.MaxHours = 16
	cfg.Blocking.AdminPIN = "1234"
	cfg.Blocking.TestUserIDs = []int64{960}
	r := &rig{api: &fakePERCo{blocked: map[int64]bool{}, active: map[int64]bool{}}, st: st,
		now: time.Date(2026, 10, 12, 7, 31, 0, 0, tz), ok: true, next: 1}
	r.b = &Blocker{cfg: cfg, api: r.api, st: st, log: log.New(io.Discard, "", 0), loc: tz,
		now: func() time.Time { return r.now }, meals: func() []meals.Meal { return live },
		healthy: func() bool { return r.ok }, notify: func() {}, self: "meal-service"}
	st.SetKV(modeKey, mode)
	return r
}

// pass simulates a fresh pass by user at the rig's current time.
func (r *rig) pass(user int64) {
	r.next++
	ev := store.Event{ID: r.next, TS: r.now.Format("2006-01-02 15:04:05"), Day: r.now.Format("2006-01-02"),
		Kind: store.KindPass, Code: 17, UserID: user, FIO: "Тест", Meal: meals.At(live, r.now)}
	r.st.InsertEvent(ev)
	r.b.OnPass(context.Background(), ev, r.now)
}

func (r *rig) operator(code int, user int64, actor string) {
	r.next++
	r.st.InsertEvent(store.Event{ID: r.next, TS: r.now.Format("2006-01-02 15:04:05"), Day: r.now.Format("2006-01-02"),
		Kind: store.KindStaffOp, Code: code, UserID: user, Actor: actor})
}

func (r *rig) tick(t *testing.T) {
	t.Helper()
	if err := r.b.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) state(t *testing.T, user int64) store.Block {
	t.Helper()
	bs, _ := r.st.BlocksOfDay(r.now.Format("2006-01-02"))
	for _, b := range bs {
		if b.UserID == user {
			return b
		}
	}
	t.Fatalf("no block row for %d", user)
	return store.Block{}
}

func TestBlockAndReleaseBeforeNextWindow(t *testing.T) {
	r := newRig(t, ModeOn)
	r.pass(900)
	if !r.api.blocked[900] || r.state(t, 900).State != store.BlockActive {
		t.Fatal("first breakfast pass must block")
	}
	if got := r.state(t, 900).RestoreAt; got != "2026-10-12 11:30:00" {
		t.Fatalf("restore at %s, want next window (lunch 12:00) - 30 min", got)
	}
	for _, h := range []int{8, 9, 10} { // window end 08:15 passes: still blocked
		r.now = time.Date(2026, 10, 12, h, 20, 0, 0, tz)
		r.tick(t)
		if !r.api.blocked[900] {
			t.Fatalf("released at %02d:20, before the next window", h)
		}
	}
	r.now = time.Date(2026, 10, 12, 11, 30, 0, 0, tz)
	r.tick(t)
	if r.api.blocked[900] || r.state(t, 900).State != store.BlockReleased {
		t.Fatal("must be released 30 min before the next window")
	}
	// Lunch is a new window: blocked again.
	r.now = time.Date(2026, 10, 12, 12, 10, 0, 0, tz)
	r.pass(900)
	if !r.api.blocked[900] {
		t.Fatal("first lunch pass must block")
	}
}

func TestModesAndKillSwitch(t *testing.T) {
	r := newRig(t, ModeOff)
	r.pass(900)
	if len(r.api.sets) != 0 {
		t.Fatal("off mode must not touch PERCo")
	}
	r.st.SetKV(modeKey, ModeTest)
	r.pass(901)
	r.pass(960)
	if r.api.blocked[901] || !r.api.blocked[960] {
		t.Fatal("test mode blocks only test_user_ids")
	}
	r.b.cfg.Blocking.ForceOff = true
	r.pass(960) // would be ignored anyway (same window), check effective mode
	if r.b.EffectiveMode() != ModeOff {
		t.Fatal("force_off must win over the dashboard")
	}
}

func TestSwitchOffReleasesEverything(t *testing.T) {
	r := newRig(t, ModeOn)
	r.pass(900)
	r.pass(901)
	rel, failed, err := r.b.SetMode(context.Background(), ModeOff, "test")
	if err != nil || rel != 2 || failed != 0 {
		t.Fatalf("SetMode off: released=%d failed=%d err=%v", rel, failed, err)
	}
	if r.api.blocked[900] || r.api.blocked[901] {
		t.Fatal("off must unblock all service blocks immediately")
	}
}

func TestFailOpen(t *testing.T) {
	r := newRig(t, ModeOn)
	r.ok = false
	r.pass(900)
	if r.api.blocked[900] {
		t.Fatal("no block without a healthy PERCo link")
	}
	r.ok = true
	r.now = time.Date(2026, 10, 12, 7, 31, 0, 0, tz)
	// Catch-up after downtime: an old breakfast pass seen at 07:50.
	r.now = time.Date(2026, 10, 12, 7, 50, 0, 0, tz)
	old := store.Event{ID: 99, TS: "2026-10-12 07:31:00", Day: "2026-10-12", Kind: store.KindPass, UserID: 902, Meal: "b"}
	r.b.OnPass(context.Background(), old, time.Date(2026, 10, 12, 7, 31, 0, 0, tz))
	if r.api.blocked[902] {
		t.Fatal("never block on stale events")
	}
	r.api.fail = errors.New("connection refused")
	r.pass(903)
	if r.state(t, 903).State != store.BlockClosed {
		t.Fatal("PERCo unreachable before blocking → closed, not blocked")
	}
}

func TestOperatorAlreadyBlocked(t *testing.T) {
	r := newRig(t, ModeOn)
	r.api.blocked[900] = true // blocked by a person before the pass
	r.pass(900)
	r.now = time.Date(2026, 10, 12, 8, 30, 0, 0, tz)
	r.tick(t)
	if !r.api.blocked[900] || len(r.api.sets) != 0 {
		t.Fatal("must never unblock someone the service did not block")
	}
}

func TestOperatorUnblocksDuringWindow(t *testing.T) {
	r := newRig(t, ModeOn)
	r.pass(900)
	r.now = r.now.Add(5 * time.Minute)
	r.api.blocked[900] = false
	r.operator(evStaffUnblocked, 900, "admin")
	r.tick(t)
	if s := r.state(t, 900); s.State != store.BlockClosed {
		t.Fatalf("state %s, want closed by operator", s.State)
	}
	r.now = r.now.Add(5 * time.Minute)
	r.pass(900) // "let them eat again": no re-block this window
	if r.api.blocked[900] {
		t.Fatal("must not re-block after an operator unblock in the same window")
	}
}

func TestOperatorBlocksOnTop(t *testing.T) {
	r := newRig(t, ModeOn)
	r.pass(900)
	r.now = r.now.Add(5 * time.Minute)
	r.operator(evStaffBlocked, 900, "admin")
	r.tick(t)
	r.now = time.Date(2026, 10, 12, 9, 0, 0, 0, tz)
	r.tick(t)
	r.b.ReleaseAll(context.Background(), "test", "проверка")
	if !r.api.blocked[900] || r.state(t, 900).State != store.BlockHeld {
		t.Fatal("operator block must survive window end and release-all")
	}
	// The operator lifts it in PERCo → row closes, nothing sent.
	r.api.blocked[900] = false
	r.tick(t)
	if r.state(t, 900).State != store.BlockClosed {
		t.Fatal("held row must close once the operator unblocks")
	}
}

func TestReleaseRetriesOnError(t *testing.T) {
	r := newRig(t, ModeOn)
	r.pass(900)
	r.now = time.Date(2026, 10, 12, 11, 30, 0, 0, tz)
	r.api.fail = errors.New("timeout")
	r.tick(t)
	s := r.state(t, 900)
	if s.State != store.BlockActive || s.Attempts != 1 || s.LastError == "" {
		t.Fatalf("failed release must stay active with an error: %+v", s)
	}
	r.api.fail = nil
	r.tick(t) // before next_try (15 s): skipped
	if r.state(t, 900).State != store.BlockActive {
		t.Fatal("retry must wait for its backoff")
	}
	r.now = r.now.Add(20 * time.Second)
	r.tick(t)
	if r.api.blocked[900] || r.state(t, 900).State != store.BlockReleased {
		t.Fatal("retry after backoff must release")
	}
}

func TestCrashBetweenIntentAndBlock(t *testing.T) {
	r := newRig(t, ModeOn)
	// Write-ahead row exists, PERCo did block, but the service died before confirming.
	r.st.CreateBlock(store.Block{UserID: 900, Day: "2026-10-12", Meal: "b", State: store.BlockPending,
		CreatedAt: "2026-10-12 07:31:00", RestoreAt: "2026-10-12 08:16:00", Deadline: "2026-10-12 10:31:00"})
	r.api.blocked[900] = true
	r.now = time.Date(2026, 10, 12, 12, 0, 0, 0, tz) // restarted after the block was due
	r.tick(t)                                        // pending → active
	r.tick(t)                                        // active and due → released
	if r.api.blocked[900] || r.state(t, 900).State != store.BlockReleased {
		t.Fatal("restart must finish and release a half-done block")
	}
}

func TestHeldUntilMidnightAfterLastWindow(t *testing.T) {
	r := newRig(t, ModeOn)
	r.now = time.Date(2026, 10, 12, 18, 40, 0, 0, tz) // dinner
	r.pass(900)
	if got := r.state(t, 900).RestoreAt; got != "2026-10-13 00:00:00" {
		t.Fatalf("restore at %s, want midnight", got)
	}
	r.now = time.Date(2026, 10, 12, 23, 30, 0, 0, tz)
	r.tick(t)
	if !r.api.blocked[900] {
		t.Fatal("released before midnight")
	}
	r.now = time.Date(2026, 10, 13, 0, 0, 0, 0, tz)
	r.tick(t)
	if r.api.blocked[900] {
		t.Fatal("must be released at midnight")
	}
}

func TestNextWindowOnlyAmongOwnTemplates(t *testing.T) {
	r := newRig(t, ModeOn)
	// Breakfast (3) and dinner (5) only: no lunch, so the block holds until dinner.
	r.st.UpsertStaff(store.Staff{ID: 900, FIO: "Тест", Templates: []int{3, 5}}, "")
	r.pass(900)
	if got := r.state(t, 900).RestoreAt; got != "2026-10-12 18:00:00" {
		t.Fatalf("restore at %s, want dinner 18:30 - 30 min", got)
	}
}

func TestAdjacentWindowsNeverReleaseInsideWindow(t *testing.T) {
	r := newRig(t, ModeOn)
	r.b.meals = func() []meals.Meal {
		return []meals.Meal{
			{Key: "b", TemplateID: 3, Intervals: []meals.Interval{{Begin: 25500, End: 29700}}},
			{Key: "l", TemplateID: 4, Intervals: []meals.Interval{{Begin: 29700, End: 48600}}},
		}
	}
	r.pass(900)
	if got := r.state(t, 900).RestoreAt; got != "2026-10-12 08:16:00" {
		t.Fatalf("restore at %s, want window end 08:15 + 1 min (never inside the window)", got)
	}
}

func TestPassOutsideWindowBlocksUntilNextWindow(t *testing.T) {
	r := newRig(t, ModeOn)
	r.now = time.Date(2026, 10, 12, 10, 0, 0, 0, tz) // between breakfast and lunch
	r.pass(900)
	if !r.api.blocked[900] {
		t.Fatal("a pass outside any window must block until the next window")
	}
	if s := r.state(t, 900); s.RestoreAt != "2026-10-12 11:30:00" || s.Meal != "out:12:00" {
		t.Fatalf("block %+v, want restore 11:30 key out:12:00", s)
	}
	r.now = time.Date(2026, 10, 12, 11, 30, 0, 0, tz)
	r.tick(t)
	if r.api.blocked[900] {
		t.Fatal("must be released 30 min before lunch")
	}
	r.pass(900) // 11:30 is outside every window but less than 30 min is left: no new block
	if r.api.blocked[900] {
		t.Fatal("no block when the next window is under 30 min away")
	}
}

func TestPassOutsideWindowAfterLastWindow(t *testing.T) {
	r := newRig(t, ModeOn)
	r.now = time.Date(2026, 10, 12, 21, 0, 0, 0, tz)
	r.pass(900)
	if s := r.state(t, 900); s.RestoreAt != "2026-10-13 00:00:00" || s.Meal != "out:24:00" {
		t.Fatalf("block %+v, want restore at midnight", s)
	}
}

func TestPassOutsideWindowOnlyTestUsersInTestMode(t *testing.T) {
	r := newRig(t, ModeTest)
	r.now = time.Date(2026, 10, 12, 10, 0, 0, 0, tz)
	r.pass(900)
	r.pass(960)
	if r.api.blocked[900] || !r.api.blocked[960] {
		t.Fatal("test mode must block only test_user_ids")
	}
}

func TestMaxHoursCap(t *testing.T) {
	r := newRig(t, ModeOn)
	r.b.cfg.Blocking.MaxHours = 0.25 // 15 min, shorter than the window
	r.pass(900)
	r.now = r.now.Add(16 * time.Minute)
	r.tick(t)
	if r.api.blocked[900] {
		t.Fatal("no block may outlive max_hours")
	}
}

func TestPIN(t *testing.T) {
	r := newRig(t, ModeOff)
	if r.b.CheckPIN("0000") || !r.b.CheckPIN("1234") {
		t.Fatal("PIN check")
	}
	r.b.cfg.Blocking.AdminPIN = ""
	if r.b.CheckPIN("") {
		t.Fatal("empty configured PIN must disable admin actions")
	}
}
