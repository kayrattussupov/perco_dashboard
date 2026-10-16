package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"perco-meal/internal/store"
)

func at(h, m int) time.Time { return time.Date(2026, 10, 12, h, m, 0, 0, tz) }

// A release is never written while the controller is off line, and succeeds once it is back.
func TestReleaseWaitsForController(t *testing.T) {
	r := newRig(t, ModeOn)
	r.pass(900) // breakfast pass at 07:31 → blocked until 11:30
	r.api.offline = true
	r.now = at(11, 31)
	r.tick(t)
	if !r.api.blocked[900] || len(r.api.sets) != 1 {
		t.Fatalf("controller off line: must not write, sets=%v", r.api.sets)
	}
	if st := r.state(t, 900); st.State != store.BlockActive || st.Attempts != 1 {
		t.Fatalf("row must stay active with a retry counted, got %s attempts=%d", st.State, st.Attempts)
	}
	r.api.offline = false
	r.now = at(11, 40)
	r.tick(t)
	if r.api.blocked[900] || r.state(t, 900).State != store.BlockReleased {
		t.Fatal("controller back: the block must be released")
	}
}

// PERCo saying "ok" is not enough: the flag must read back as cleared.
func TestReleaseNeedsReadBack(t *testing.T) {
	r := newRig(t, ModeOn)
	r.pass(900)
	r.api.ignoreUnlock = true
	r.now = at(11, 31)
	r.tick(t)
	st := r.state(t, 900)
	if st.State != store.BlockActive || !strings.Contains(st.LastError, "не подтвердил") {
		t.Fatalf("unconfirmed unblock must stay open with an error, got %s %q", st.State, st.LastError)
	}
	r.api.ignoreUnlock = false
	r.now = at(11, 40)
	r.tick(t)
	if r.state(t, 900).State != store.BlockReleased {
		t.Fatal("must release once PERCo really clears the flag")
	}
}

// A write is "unsettled" until the controller has taken it over; the warning appears if that drags on.
func TestSettleMarkerAndWarning(t *testing.T) {
	r := newRig(t, ModeOn)
	r.pass(900)
	r.api.pending = 3 // PERCo still has commands for the controller
	r.now = at(7, 33)
	r.tick(t)
	if r.st.GetKV(settleKey) == "" {
		t.Fatal("a block write must leave the unsettled mark while commands are pending")
	}
	if s, _ := r.b.Status(); len(s.Warnings) != 0 {
		t.Fatalf("no warning right after the write: %v", s.Warnings)
	}
	r.now = at(7, 40)
	s, _ := r.b.Status()
	if len(s.Warnings) == 0 || !strings.Contains(strings.Join(s.Warnings, " "), "не подтверждены контроллером") {
		t.Fatalf("expected an unsettled warning after %v, got %v", settleWarn, s.Warnings)
	}
	r.api.pending = 0
	r.tick(t)
	if r.st.GetKV(settleKey) != "" {
		t.Fatal("mark must clear once the controller has everything")
	}
}

func checkRig(t *testing.T) *rig {
	r := newRig(t, ModeOn)
	r.b.cfg.SelfCheck.Enabled = true
	r.b.cfg.SelfCheck.Times = []string{"20:05"}
	r.now = at(20, 6)
	return r
}

func (r *rig) selfCheck(t *testing.T) CheckRecord {
	t.Helper()
	if err := r.b.SelfCheckTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok := r.b.CheckResult("20:05")
	if !ok {
		t.Fatal("check did not record a result")
	}
	return rec
}

func TestSelfCheckHealthy(t *testing.T) {
	r := checkRig(t)
	rec := r.selfCheck(t)
	if !rec.OK || rec.Late || len(rec.Problems) != 0 {
		t.Fatalf("healthy system: %+v", rec)
	}
	ran := rec.RanAt
	r.now = at(20, 10)
	if got := r.selfCheck(t); got.RanAt != ran {
		t.Fatal("a check runs once per due moment")
	}
}

func TestSelfCheckNotYetDue(t *testing.T) {
	r := checkRig(t)
	r.now = at(20, 4)
	r.b.SelfCheckTick(context.Background())
	if _, ok := r.b.CheckResult("20:05"); ok {
		t.Fatal("must not run before the due time")
	}
}

// Missed because the PC was off: runs at start, marked late.
func TestSelfCheckLateAfterDowntime(t *testing.T) {
	r := checkRig(t)
	r.now = at(22, 0)
	if rec := r.selfCheck(t); !rec.Late || !rec.OK {
		t.Fatalf("expected a late but clean check, got %+v", rec)
	}
}

func TestSelfCheckWaitsForControllerThenReports(t *testing.T) {
	r := checkRig(t)
	r.api.offline = true
	r.b.SelfCheckTick(context.Background())
	if _, ok := r.b.CheckResult("20:05"); ok {
		t.Fatal("within the grace period the check waits for the controller")
	}
	r.now = at(20, 40)
	rec := r.selfCheck(t)
	if rec.OK || !strings.Contains(strings.Join(rec.Problems, " "), "не на связи") {
		t.Fatalf("after the grace period the outage is the finding: %+v", rec)
	}
}

func TestSelfCheckFindsStrayAndUnknownCards(t *testing.T) {
	r := checkRig(t)
	for id, fio := range map[int64]string{777: "Ручной Блок", 778: "Свой Блок", 779: "Здоров Здоров"} {
		if err := r.st.UpsertStaff(store.Staff{ID: id, FIO: fio, Active: true}, ""); err != nil {
			t.Fatal(err)
		}
	}
	r.api.blocked[777] = true // blocked by a person, no row of the service
	r.now = at(19, 10)
	r.pass(778) // the service's own dinner block (due 19:31, released by the check): never "stray"
	r.next++
	r.st.InsertEvent(store.Event{ID: r.next, TS: "2026-10-12 19:20:00", Day: "2026-10-12", Kind: store.KindDeny,
		Code: 1, UserID: 779, FIO: "Здоров Здоров", Meal: "d"})
	r.now = at(20, 6)
	rec := r.selfCheck(t)
	joined := strings.Join(rec.Problems, " | ")
	if rec.OK || !strings.Contains(joined, "Ручной Блок") || strings.Contains(joined, "Свой Блок") {
		t.Fatalf("stray block must be reported, the service's own must not: %q", joined)
	}
	if !strings.Contains(joined, "не знает карты") || !strings.Contains(joined, "Здоров Здоров") {
		t.Fatalf("card unknown to the controller must be reported: %q", joined)
	}
}

// An overdue block of the service that cannot be released shows up as a problem, not silently.
func TestSelfCheckReportsStuckOwnBlock(t *testing.T) {
	r := newRig(t, ModeOn)
	r.b.cfg.SelfCheck.Enabled = true
	r.b.cfg.SelfCheck.Times = []string{"11:35"}
	r.pass(900) // until 11:30
	r.api.ignoreUnlock = true
	r.now = at(11, 36)
	r.b.SelfCheckTick(context.Background())
	rec, ok := r.b.CheckResult("11:35")
	if !ok || rec.OK || !strings.Contains(strings.Join(rec.Problems, " "), "Не снята блокировка") {
		t.Fatalf("stuck block must fail the check: %+v ok=%v", rec, ok)
	}
}

// The lost-card problem lasts until the person passes successfully, not until the next check.
func TestUnknownCardClearsAfterSuccessfulPass(t *testing.T) {
	r := checkRig(t)
	r.st.UpsertStaff(store.Staff{ID: 779, FIO: "Здоров Здоров", Active: true}, "")
	r.next++
	r.st.InsertEvent(store.Event{ID: r.next, TS: "2026-10-12 19:20:00", Day: "2026-10-12", Kind: store.KindDeny,
		Code: 1, UserID: 779, FIO: "Здоров Здоров", Meal: "d"})

	r.now = at(20, 6)
	if got := strings.Join(r.b.unknownCards(r.now), " "); !strings.Contains(got, "Здоров Здоров") {
		t.Fatalf("rejected and not passed since: must be reported, got %q", got)
	}
	r.now = at(20, 30) // a later check with nobody swiping must not turn it green
	if got := r.b.unknownCards(r.now); len(got) == 0 {
		t.Fatal("no swipe since the rejection: the problem must persist")
	}

	r.next++
	r.st.InsertEvent(store.Event{ID: r.next, TS: "2026-10-12 20:40:00", Day: "2026-10-12", Kind: store.KindPass,
		Code: 17, UserID: 779, FIO: "Здоров Здоров", Meal: "d"})
	r.now = at(20, 45)
	if got := r.b.unknownCards(r.now); len(got) != 0 {
		t.Fatalf("passed successfully after the rejection: the card works, got %v", got)
	}
}

// A later clean check supersedes an earlier failed one on the dashboard.
func TestLatestCheckSupersedesEarlierFailure(t *testing.T) {
	r := newRig(t, ModeOn)
	r.b.cfg.SelfCheck.Enabled = true
	r.b.cfg.SelfCheck.Times = []string{"06:30", "06:45"}
	save := func(hm, ran string, ok bool, problems ...string) {
		rec := CheckRecord{Time: hm, Due: "2026-10-12 " + hm + ":00", RanAt: ran, OK: ok, Problems: problems}
		raw, _ := json.Marshal(rec)
		r.st.SetKV(checkKeyPrefix+hm, string(raw))
	}
	r.now = at(7, 0)
	save("06:30", "2026-10-12 06:30:05", false, "что-то не так")
	save("06:45", "2026-10-12 06:45:05", true)
	s, _ := r.b.Status()
	if strings.Contains(strings.Join(s.Warnings, " "), "Проверка") {
		t.Fatalf("06:45 was clean and later: no check warning expected, got %v", s.Warnings)
	}
	save("06:45", "2026-10-12 06:45:05", false, "всё ещё не так")
	s, _ = r.b.Status()
	if !strings.Contains(strings.Join(s.Warnings, " "), "всё ещё не так") {
		t.Fatalf("the latest failed check must warn, got %v", s.Warnings)
	}
}
