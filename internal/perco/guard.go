package perco

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Protection of PERCo-Web from this service: every request goes through a concurrency limit, a request budget
// and a circuit breaker, so no error, burst or bug turns into a flood of requests to a server that already struggles.
const (
	maxInFlight    = 2                // requests to PERCo at the same time
	breakAfter     = 3                // consecutive failures that open the breaker
	breakPause     = 30 * time.Second // no requests at all for this long once open
	urgentEvery    = time.Second      // budget of urgent reads: events, link state, an employee's block flag
	urgentBurst    = 3
	backgroundEvery = 2 * time.Second // budget of background reads: staff, meal windows, blocked list
	backgroundBurst = 2
	metricsWindow  = time.Hour
)

// ErrPaused is returned without contacting PERCo while the breaker is open.
var ErrPaused = errors.New("PERCo не отвечает: запросы приостановлены")

type priorityKey struct{}

// Background marks requests that may wait: they get the smaller budget and never delay urgent ones.
func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, priorityKey{}, true)
}

func isBackground(ctx context.Context) bool { b, _ := ctx.Value(priorityKey{}).(bool); return b }

type unbudgetedKey struct{}

// Unbudgeted skips the request budget (the breaker and the concurrency limit still apply). Only for perco-bench,
// which paces its own load test and must measure PERCo, not this client's budget.
func Unbudgeted(ctx context.Context) context.Context {
	return context.WithValue(ctx, unbudgetedKey{}, true)
}

func isUnbudgeted(ctx context.Context) bool { b, _ := ctx.Value(unbudgetedKey{}).(bool); return b }

// bucket is a token bucket: on average one request per every, at most burst at once.
type bucket struct {
	mu     sync.Mutex
	every  time.Duration
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(every time.Duration, burst int) *bucket {
	return &bucket{every: every, burst: float64(burst), tokens: float64(burst)}
}

// wait takes one token, sleeping until one is available or ctx ends.
func (b *bucket) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := time.Now()
		if !b.last.IsZero() {
			b.tokens = min(b.burst, b.tokens+float64(now.Sub(b.last))/float64(b.every))
		}
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		need := time.Duration((1 - b.tokens) * float64(b.every))
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(need):
		}
	}
}

// breaker opens after breakAfter consecutive failures; after breakPause it lets one probe through.
type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
	probing   bool
	opened    int // times opened, for the dashboard
}

func (b *breaker) allow(now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures < breakAfter {
		return nil
	}
	if now.Before(b.openUntil) || b.probing {
		return fmt.Errorf("%w до %s", ErrPaused, b.openUntil.Format("15:04:05"))
	}
	b.probing = true // half-open: this request is the probe
	return nil
}

// release ends a request that was cancelled by the caller: it says nothing about PERCo.
func (b *breaker) release() {
	b.mu.Lock()
	b.probing = false
	b.mu.Unlock()
}

func (b *breaker) done(now time.Time, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if !failed {
		b.failures = 0
		return
	}
	b.failures++
	if b.failures >= breakAfter {
		if b.failures == breakAfter {
			b.opened++
		}
		b.openUntil = now.Add(breakPause)
	}
}

// sample is one finished request, kept for metricsWindow.
type sample struct {
	at     time.Time
	kind   string
	took   time.Duration
	failed bool
}

type metrics struct {
	mu      sync.Mutex
	samples []sample
	paused  int // requests refused by the open breaker
}

func (m *metrics) add(s sample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cut := 0
	for cut < len(m.samples) && s.at.Sub(m.samples[cut].at) > metricsWindow {
		cut++
	}
	m.samples = append(m.samples[cut:], s)
}

// KindStats are the request figures of one kind of request over the last hour.
type KindStats struct {
	Kind          string  `json:"kind"`
	LastMinute    int     `json:"last_minute"`
	LastHour      int     `json:"last_hour"`
	Errors     int     `json:"errors"` // last hour
	P50s       float64 `json:"p50_s"`
	P95s       float64 `json:"p95_s"`
	P99s       float64 `json:"p99_s"`
}

// Stats is what the dashboard shows about the service's load on PERCo.
type Stats struct {
	LastMinute  int         `json:"last_minute"`
	LastHour    int         `json:"last_hour"`
	ErrorsHour  int         `json:"errors_hour"`
	Paused      int         `json:"paused"` // requests not sent because the breaker was open (since start)
	BreakerOpen bool        `json:"breaker_open"`
	OpenUntil   string      `json:"open_until,omitempty"`
	TimesOpened int         `json:"times_opened"`
	Kinds       []KindStats `json:"kinds"`
}

func (m *metrics) stats(now time.Time) Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	var st Stats
	by := map[string][]sample{}
	for _, s := range m.samples {
		if now.Sub(s.at) > metricsWindow {
			continue
		}
		by[s.kind] = append(by[s.kind], s)
		st.LastHour++
		if now.Sub(s.at) <= time.Minute {
			st.LastMinute++
		}
		if s.failed {
			st.ErrorsHour++
		}
	}
	st.Paused = m.paused
	for kind, ss := range by {
		ks := KindStats{Kind: kind, LastHour: len(ss)}
		var took []time.Duration
		for _, s := range ss {
			if now.Sub(s.at) <= time.Minute {
				ks.LastMinute++
			}
			if s.failed {
				ks.Errors++
			} else {
				took = append(took, s.took)
			}
		}
		slices.Sort(took)
		pct := func(p int) float64 {
			if len(took) == 0 {
				return 0
			}
			return took[(len(took)-1)*p/100].Seconds()
		}
		ks.P50s, ks.P95s, ks.P99s = pct(50), pct(95), pct(99)
		st.Kinds = append(st.Kinds, ks)
	}
	slices.SortFunc(st.Kinds, func(a, b KindStats) int { return b.LastHour - a.LastHour })
	return st
}
