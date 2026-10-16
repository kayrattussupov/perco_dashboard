package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"perco-meal/internal/perco"
	"perco-meal/internal/store"
)

func (r *rig) health(t *testing.T) ControllerStatus {
	t.Helper()
	if err := r.b.HealthLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r.b.Controller()
}

func TestControllerStatusStates(t *testing.T) {
	r := newRig(t, ModeOn)
	changes := 0
	r.b.notify = func() { changes++ }

	if st := r.b.Controller(); st.State != StateNoData {
		t.Fatalf("before the first reading: %s, want %s", st.State, StateNoData)
	}
	if st := r.health(t); st.State != StateOK || changes != 1 {
		t.Fatalf("healthy: %+v, changes=%d", st, changes)
	}
	r.health(t)
	if changes != 1 {
		t.Fatalf("an unchanged reading must not push an update, changes=%d", changes)
	}

	r.api.pending = 2
	r.now = r.now.Add(5 * time.Second)
	if st := r.health(t); st.State != StateSyncing || st.Pending != 2 {
		t.Fatalf("pending commands: %+v", st)
	}

	r.api.pending, r.api.offline = 0, true
	r.now = r.now.Add(5 * time.Second)
	st := r.health(t)
	if st.State != StateOffline || !strings.Contains(st.Text, "не на связи") {
		t.Fatalf("controller off line: %+v", st)
	}
	since := st.Since
	r.now = r.now.Add(time.Minute)
	if st := r.health(t); st.Since != since {
		t.Fatalf("outage start must not move while it lasts: %s -> %s", since, st.Since)
	}

	r.api.offline, r.api.fail = false, errors.New("connection refused")
	r.now = r.now.Add(5 * time.Second)
	if st := r.health(t); st.State != StateNoPERCo || !strings.Contains(st.Text, "connection refused") {
		t.Fatalf("PERCo-Web unreachable: %+v", st)
	}

	r.api.fail = nil
	r.now = r.now.Add(5 * time.Second)
	if st := r.health(t); st.State != StateOK {
		t.Fatalf("recovered: %+v", st)
	}

	r.now = r.now.Add(healthStale + time.Second) // the loop has stopped: old data is not shown as current
	if st := r.b.Controller(); st.State != StateNoData {
		t.Fatalf("stale reading: %s", st.State)
	}
}

func TestOfflineControllerWarning(t *testing.T) {
	r := newRig(t, ModeOn)
	r.api.offline = true
	r.health(t)
	s, err := r.b.Status()
	if err != nil {
		t.Fatal(err)
	}
	if s.Controller.State != StateOffline || !strings.Contains(strings.Join(s.Warnings, " "), "Контроллер не на связи") {
		t.Fatalf("an outage must be a dashboard warning: %+v %v", s.Controller, s.Warnings)
	}
}

// The write gate reuses the background reading: a burst of passes costs no extra PERCo requests.
func TestLinkUpReusesBackgroundReading(t *testing.T) {
	r := newRig(t, ModeOn)
	r.health(t)
	calls := r.api.healthCalls
	for i := 0; i < 20; i++ {
		if !r.b.linkUp(context.Background()) {
			t.Fatal("link must be up")
		}
	}
	if r.api.healthCalls != calls {
		t.Fatalf("linkUp made %d extra requests within the cache window", r.api.healthCalls-calls)
	}
	r.now = r.now.Add(linkCacheTTL + time.Second)
	r.b.linkUp(context.Background())
	if r.api.healthCalls != calls+1 {
		t.Fatal("an expired reading must be read again")
	}
}

// "Blocked now" counts everyone blocked in PERCo, splitting the service's own from manual ones.
func TestBlockedNowCountsEveryoneBlocked(t *testing.T) {
	r := newRig(t, ModeOn)
	r.st.UpsertStaff(store.Staff{ID: 777, FIO: "Ручной Блок", Active: true}, "")
	if s, _ := r.b.Status(); s.Blocked.Known {
		t.Fatal("no reading yet: the count must not be presented as known")
	}
	r.pass(900)               // the service blocks 900
	r.api.blocked[777] = true // an operator blocked 777
	if err := r.b.BlockedLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := r.b.Status()
	if err != nil {
		t.Fatal(err)
	}
	bn := s.Blocked
	if !bn.Known || bn.Count != 2 || bn.Service != 1 || bn.Manual != 1 || len(bn.Names) != 1 || bn.Names[0] != "Ручной Блок" {
		t.Fatalf("blocked now: %+v", bn)
	}

	r.api.blocked[777], r.api.blocked[900] = false, false
	r.b.BlockedLoop(context.Background())
	if s, _ := r.b.Status(); s.Blocked.Count != 0 {
		t.Fatalf("nobody blocked: %+v", s.Blocked)
	}

	r.api.fail = errors.New("нет связи")
	r.b.BlockedLoop(context.Background())
	if s, _ := r.b.Status(); s.Blocked.Error == "" || !s.Blocked.Known {
		t.Fatalf("a failed reading keeps the last numbers and reports the error: %+v", s.Blocked)
	}
}

// While PERCo-Web is unreachable the whole staff table is not read: it would only add load to a failing server.
func TestBlockedLoopSkipsWhilePERCoDown(t *testing.T) {
	r := newRig(t, ModeOn)
	r.b.noteHealth(perco.DeviceHealth{}, errors.New("нет сети"))
	r.api.blocked[777] = true
	r.b.BlockedLoop(context.Background())
	if s, _ := r.b.Status(); s.Blocked.Known {
		t.Fatalf("PERCo is down: the table must not be read: %+v", s.Blocked)
	}
	r.b.noteHealth(perco.DeviceHealth{Connected: true}, nil)
	r.b.BlockedLoop(context.Background())
	if s, _ := r.b.Status(); !s.Blocked.Known || s.Blocked.Count != 1 {
		t.Fatalf("PERCo is back: %+v", s.Blocked)
	}
}

// The background loop costs one request; the per-device error lists are read only every errorsEvery
// and are kept in between.
func TestHealthLoopReadsErrorsRarely(t *testing.T) {
	r := newRig(t, ModeOn)
	r.api.devErrors = []string{"Карта 123"}
	r.health(t) // first reading is full
	full := r.api.healthCalls
	for i := 0; i < 5; i++ {
		r.now = r.now.Add(healthEvery)
		if st := r.health(t); len(st.Errors) != 1 {
			t.Fatalf("errors of the last full reading must be kept: %+v", st)
		}
	}
	if r.api.healthCalls != full+5 {
		t.Fatalf("state-only readings expected: %d calls", r.api.healthCalls-full)
	}
	if r.api.fullCalls != 1 {
		t.Fatalf("errors read %d times within %s", r.api.fullCalls, errorsEvery)
	}
	r.now = r.now.Add(errorsEvery)
	r.health(t)
	if r.api.fullCalls != 2 {
		t.Fatal("errors must be re-read after errorsEvery")
	}
}

// After an outage writes wait until the link has stayed up for linkStable; a new break restarts the wait.
func TestLinkMustBeStableBeforeWrites(t *testing.T) {
	r := newRig(t, ModeOn)
	stable := 2 * time.Minute
	r.b.linkStable = &stable
	ctx := context.Background()
	r.health(t)
	if r.b.linkUp(ctx) {
		t.Fatal("a link seen for the first time is not yet stable")
	}
	if st := r.b.Controller(); st.State != StateWaiting {
		t.Fatalf("state %s, want %s", st.State, StateWaiting)
	}
	r.now = r.now.Add(time.Minute)
	r.api.offline = true
	r.health(t)
	r.now = r.now.Add(30 * time.Second)
	r.api.offline = false
	r.health(t)
	r.now = r.now.Add(90 * time.Second)
	if r.b.linkUp(ctx) {
		t.Fatal("the break restarted the wait: 90 s is not enough")
	}
	r.now = r.now.Add(31 * time.Second)
	if !r.b.linkUp(ctx) {
		t.Fatal("link up for over 2 min must allow writes")
	}
	r.pass(900)
	if !r.api.blocked[900] {
		t.Fatal("a pass on a stable link must block")
	}
}

// While the controller is off line a burst of passes reuses the negative reading instead of asking PERCo each time.
func TestLinkDownReadingIsReused(t *testing.T) {
	r := newRig(t, ModeOn)
	r.api.offline = true
	r.health(t)
	calls := r.api.healthCalls
	for i := 0; i < 10; i++ {
		r.b.linkUp(context.Background())
	}
	if r.api.healthCalls != calls {
		t.Fatalf("%d extra requests while off line", r.api.healthCalls-calls)
	}
}
