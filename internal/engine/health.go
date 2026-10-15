package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"perco-meal/internal/perco"
)

const (
	healthEvery  = 3 * time.Second  // how often the PERCo ↔ controller link is read in the background
	healthStale  = 30 * time.Second // older data is shown as "no data"
	linkCacheTTL = 3 * time.Second  // a "controller connected" reading is reused this long by the write gate
)

// States of the PERCo ↔ controller link shown on the dashboard.
const (
	StateOK      = "ok"
	StateSyncing = "syncing"     // connected, PERCo still delivering changes to the controller
	StateOffline = "offline"     // PERCo-Web is up, the controller is not connected to it
	StateNoPERCo = "unreachable" // this service cannot reach PERCo-Web
	StateNoData  = "unknown"     // no fresh reading (the service has just started)
)

// healthState is the last reading of the link, guarded by Blocker.hmu.
type healthState struct {
	h     perco.DeviceHealth
	err   string
	at    time.Time
	key   string    // state + details; a change of key is logged and pushed to the dashboard
	since time.Time // when the current state began
}

// ControllerStatus is what the dashboard shows about PERCo and the controller.
type ControllerStatus struct {
	State     string   `json:"state"`
	Text      string   `json:"text"`    // one line for the header and tooltip
	Message   string   `json:"message"` // controller status text from PERCo, if any
	Pending   int      `json:"pending"` // commands PERCo has not delivered yet
	Errors    []string `json:"errors"`  // resources not transferred to the devices
	CheckedAt string   `json:"checked_at"`
	Since     string   `json:"since"` // when this state began
}

// HealthLoop reads the link state in the background. It never returns an error: a failed reading is itself the
// state shown on the dashboard (and logged once when it changes), not something to retry noisily.
func (b *Blocker) HealthLoop(ctx context.Context) error {
	h, err := b.api.DeviceHealth(ctx, b.cfg.DeviceIDs)
	if ctx.Err() != nil {
		return nil
	}
	b.noteHealth(h, err)
	return nil
}

// noteHealth stores a reading. A change of state is logged and pushed to the dashboard at once.
func (b *Blocker) noteHealth(h perco.DeviceHealth, err error) {
	now := b.now()
	b.hmu.Lock()
	b.health.at = now
	b.health.h, b.health.err = h, ""
	if err != nil {
		b.health.h, b.health.err = perco.DeviceHealth{}, err.Error()
	}
	st := statusOf(b.health, now)
	key := st.State + "|" + st.Message + "|" + strings.Join(st.Errors, ",")
	changed := key != b.health.key
	if changed {
		if prev, _, _ := strings.Cut(b.health.key, "|"); prev != st.State {
			b.health.since = now
		}
		b.health.key = key
	}
	b.hmu.Unlock()
	if changed {
		b.log.Printf("связь PERCo: %s", st.Text)
		b.notify()
	}
}

// linkUp reports whether PERCo-Web is connected to the controllers. Writes to PERCo are made only then:
// a change accepted while the controller is off line reaches it later, out of order, and cards can be lost.
// The background reading is reused for linkCacheTTL, so a burst of passes or a mass release pays no extra
// PERCo request per employee; only a positive reading is reused.
func (b *Blocker) linkUp(ctx context.Context) bool {
	now := b.now()
	b.hmu.Lock()
	age := now.Sub(b.health.at)
	fresh := b.health.err == "" && b.health.h.Connected && age >= 0 && age < linkCacheTTL
	b.hmu.Unlock()
	if fresh {
		return true
	}
	h, err := b.api.DeviceHealth(ctx, b.cfg.DeviceIDs)
	b.noteHealth(h, err)
	return err == nil && h.Connected
}

// Controller returns the current link state for the dashboard.
func (b *Blocker) Controller() ControllerStatus {
	b.hmu.Lock()
	defer b.hmu.Unlock()
	return statusOf(b.health, b.now())
}

func statusOf(s healthState, now time.Time) ControllerStatus {
	st := ControllerStatus{Errors: []string{}, Message: s.h.Message, Pending: s.h.Pending}
	st.Errors = append(st.Errors, s.h.Errors...)
	if !s.at.IsZero() {
		st.CheckedAt = s.at.Format(perco.TimeLayout)
	}
	st.Since = st.CheckedAt
	if !s.since.IsZero() {
		st.Since = s.since.Format(perco.TimeLayout)
	}
	switch {
	case s.at.IsZero() || now.Sub(s.at) > healthStale:
		st.State, st.Text = StateNoData, "PERCo: нет свежих данных о связи"
	case s.err != "":
		st.State, st.Text = StateNoPERCo, "PERCo-Web недоступен: "+s.err
	case !s.h.Connected:
		st.State, st.Text = StateOffline, "Контроллер не на связи с PERCo-Web"+withMsg(s.h.Message)
	case s.h.Pending > 0 || len(s.h.Errors) > 0:
		st.State = StateSyncing
		st.Text = fmt.Sprintf("Контроллер на связи, идёт передача изменений (в очереди: %d", s.h.Pending)
		if len(s.h.Errors) > 0 {
			st.Text += ", не передано: " + strings.Join(s.h.Errors, ", ")
		}
		st.Text += ")"
	default:
		st.State, st.Text = StateOK, "PERCo и контроллер на связи"
	}
	return st
}
