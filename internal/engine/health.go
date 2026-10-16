package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"perco-meal/internal/perco"
)

const (
	healthEvery  = 30 * time.Second // how often the PERCo ↔ controller link is read in the background (1 request)
	healthStale  = 90 * time.Second // older data is shown as "no data"
	linkCacheTTL = 5 * time.Second  // a reading is reused this long by the write gate
	errorsEvery  = 5 * time.Minute  // the not-transferred resources (1 request per device) are re-read this often
	linkStable   = 2 * time.Minute  // writes resume only after the link has been up this long (it flapped on 14–15.10)
)

// States of the PERCo ↔ controller link shown on the dashboard.
const (
	StateOK      = "ok"
	StateSyncing = "syncing"     // connected, PERCo still delivering changes to the controller
	StateWaiting = "settling"    // connected again, waiting for the link to prove stable before writing
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

	errorsAt time.Time // when the not-transferred resources were last read
	upSince  time.Time // since when the controller has been connected without a break; zero while it is not
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
// Usually it is one request (the link state); the per-device error lists are re-read every errorsEvery.
func (b *Blocker) HealthLoop(ctx context.Context) error {
	b.hmu.Lock()
	full := b.now().Sub(b.health.errorsAt) >= errorsEvery
	b.hmu.Unlock()
	if full {
		h, err := b.api.DeviceHealth(ctx, b.cfg.DeviceIDs)
		if ctx.Err() == nil {
			b.noteHealth(h, err)
		}
		return nil
	}
	h, err := b.api.DeviceState(ctx)
	if ctx.Err() == nil {
		b.noteState(h, err)
	}
	return nil
}

// noteHealth stores a full reading (state and error lists). A change of state is logged and pushed at once.
func (b *Blocker) noteHealth(h perco.DeviceHealth, err error) { b.note(h, err, true) }

// noteState stores a state-only reading; the error lists of the last full reading are kept.
func (b *Blocker) noteState(h perco.DeviceHealth, err error) { b.note(h, err, false) }

func (b *Blocker) note(h perco.DeviceHealth, err error, full bool) {
	now := b.now()
	b.hmu.Lock()
	if !full {
		h.Errors = b.health.h.Errors
	}
	b.health.at = now
	b.health.h, b.health.err = h, ""
	if err != nil {
		b.health.h, b.health.err = perco.DeviceHealth{}, err.Error()
	} else if full {
		b.health.errorsAt = now
	}
	if err == nil && h.Connected {
		if b.health.upSince.IsZero() {
			b.health.upSince = now
		}
	} else {
		b.health.upSince = time.Time{}
	}
	st := statusOf(b.health, now, b.stableFor())
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
// The link must also have been up for linkStable without a break: a flapping link loses changes.
// Any reading is reused for linkCacheTTL, so a burst of passes or a mass release pays no extra
// PERCo request per employee, also while the controller is off line.
func (b *Blocker) linkUp(ctx context.Context) bool {
	now := b.now()
	b.hmu.Lock()
	age := now.Sub(b.health.at)
	fresh := !b.health.at.IsZero() && age >= 0 && age < linkCacheTTL
	b.hmu.Unlock()
	if !fresh {
		h, err := b.api.DeviceState(ctx)
		b.noteState(h, err)
	}
	b.hmu.Lock()
	defer b.hmu.Unlock()
	return linkReady(b.health, b.now(), b.stableFor())
}

// stableFor is how long the link must stay up before writes; tests set Blocker.linkStable to 0.
func (b *Blocker) stableFor() time.Duration {
	if b.linkStable != nil {
		return *b.linkStable
	}
	return linkStable
}

func linkReady(s healthState, now time.Time, stable time.Duration) bool {
	return s.err == "" && s.h.Connected && !s.upSince.IsZero() && now.Sub(s.upSince) >= stable
}

// Controller returns the current link state for the dashboard.
func (b *Blocker) Controller() ControllerStatus {
	b.hmu.Lock()
	defer b.hmu.Unlock()
	return statusOf(b.health, b.now(), b.stableFor())
}

func statusOf(s healthState, now time.Time, stable time.Duration) ControllerStatus {
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
	case !linkReady(s, now, stable):
		left := stable - now.Sub(s.upSince)
		st.State = StateWaiting
		st.Text = fmt.Sprintf("Контроллер снова на связи, запись в PERCo через %d с, если связь не прервётся", int(left.Seconds()+0.5))
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
