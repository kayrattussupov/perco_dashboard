package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

	r.now = r.now.Add(time.Minute) // the loop has stopped: old data is not shown as current
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
