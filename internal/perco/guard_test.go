package perco

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer answers auth with a token and /devices/state with status (or 500 while failing).
func fakeServer(t *testing.T, failing *atomic.Bool, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/system/auth" {
			w.Write([]byte(`{"token":"t"}`))
			return
		}
		hits.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"items":[{"id":1,"connect_state":1,"cmd_count":0}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// After breakAfter failures in a row PERCo is not contacted at all until the pause ends; then one probe goes
// through, and a success closes the breaker.
func TestBreakerStopsRequestsToFailingPERCo(t *testing.T) {
	var failing atomic.Bool
	var hits atomic.Int32
	failing.Store(true)
	c := New(fakeServer(t, &failing, &hits).URL, "u", "p")
	ctx := Unbudgeted(context.Background())
	for range breakAfter {
		if _, err := c.DeviceState(ctx); err == nil {
			t.Fatal("HTTP 500 must be an error")
		}
	}
	before := hits.Load()
	for range 20 {
		if _, err := c.DeviceState(ctx); !errors.Is(err, ErrPaused) {
			t.Fatalf("open breaker must refuse at once, got %v", err)
		}
	}
	if hits.Load() != before {
		t.Fatalf("%d requests reached PERCo while paused", hits.Load()-before)
	}
	if st := c.Stats(); !st.BreakerOpen || st.Paused != 20 || st.TimesOpened != 1 {
		t.Fatalf("stats: %+v", st)
	}

	failing.Store(false)
	c.br.mu.Lock()
	c.br.openUntil = time.Now().Add(-time.Second) // the pause is over
	c.br.mu.Unlock()
	if _, err := c.DeviceState(ctx); err != nil {
		t.Fatalf("probe after the pause: %v", err)
	}
	if _, err := c.DeviceState(ctx); err != nil {
		t.Fatalf("closed after a good probe: %v", err)
	}
}

// A failed probe re-opens the breaker for another pause.
func TestBreakerFailedProbeReopens(t *testing.T) {
	var failing atomic.Bool
	var hits atomic.Int32
	failing.Store(true)
	c := New(fakeServer(t, &failing, &hits).URL, "u", "p")
	ctx := Unbudgeted(context.Background())
	for range breakAfter {
		c.DeviceState(ctx)
	}
	c.br.mu.Lock()
	c.br.openUntil = time.Now().Add(-time.Second)
	c.br.mu.Unlock()
	c.DeviceState(ctx) // probe fails
	if _, err := c.DeviceState(ctx); !errors.Is(err, ErrPaused) {
		t.Fatalf("a failed probe must pause again, got %v", err)
	}
}

// A request cancelled by the caller says nothing about PERCo: it neither opens nor resets the breaker.
func TestCancelledRequestIsNotAFailure(t *testing.T) {
	var b breaker
	now := time.Now()
	b.done(now, true)
	b.done(now, true)
	if err := b.allow(now); err != nil {
		t.Fatal(err)
	}
	b.release()
	b.done(now, true) // third real failure opens it: the cancel did not reset the count
	if err := b.allow(now); !errors.Is(err, ErrPaused) {
		t.Fatalf("want paused, got %v", err)
	}
}

// Urgent reads: urgentBurst at once, then one per urgentEvery.
func TestBudgetSpacesReads(t *testing.T) {
	var failing atomic.Bool
	var hits atomic.Int32
	c := New(fakeServer(t, &failing, &hits).URL, "u", "p")
	ctx := context.Background()
	start := time.Now()
	for range urgentBurst + 1 {
		if _, err := c.DeviceState(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took < urgentEvery*8/10 {
		t.Fatalf("%d reads took %s: the budget did not hold the extra one", urgentBurst+1, took)
	}
}

// Background reads have their own, smaller budget and do not use up the urgent one.
func TestBackgroundDoesNotDelayUrgent(t *testing.T) {
	var failing atomic.Bool
	var hits atomic.Int32
	c := New(fakeServer(t, &failing, &hits).URL, "u", "p")
	bg := Background(context.Background())
	for range backgroundBurst {
		c.DeviceState(bg)
	}
	start := time.Now()
	for range urgentBurst {
		if _, err := c.DeviceState(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("urgent reads waited %s behind background ones", took)
	}
}

func TestStatsByKind(t *testing.T) {
	var failing atomic.Bool
	var hits atomic.Int32
	c := New(fakeServer(t, &failing, &hits).URL, "u", "p")
	ctx := Unbudgeted(context.Background())
	c.DeviceState(ctx)
	c.DeviceState(ctx)
	st := c.Stats()
	if st.LastMinute != 3 || st.ErrorsHour != 0 { // auth + 2 reads
		t.Fatalf("stats: %+v", st)
	}
	if st.Kinds[0].Kind != "GET /devices/state" || st.Kinds[0].LastHour != 2 {
		t.Fatalf("kinds: %+v", st.Kinds)
	}
	if kindOf("GET", "/users/staff/960") != "GET /users/staff/{id}" {
		t.Fatal("ids in the path must be folded")
	}
}
