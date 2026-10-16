package main

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"perco-meal/internal/perco"
)

// fakePERCo applies a write storeLag after it is sent and keeps it in the controller queue for deliverLag more.
// Only the "comma" encoding works for several ids; "json" blocks a stranger to exercise the safety check.
type fakePERCo struct {
	mu         sync.Mutex
	clock      *time.Time
	storeLag   time.Duration
	deliverLag time.Duration
	blocked    map[int64]bool
	pendingTo  time.Time
	pending    int
	offline    bool
	failWrites bool
	eventsLag  time.Duration
	writes     int
	strangerOn string // format that blocks id 1 by mistake
	queued     []queued
}

type queued struct {
	at      time.Time
	ids     []int64
	blocked bool
}

func newFake(clock *time.Time) *fakePERCo {
	return &fakePERCo{clock: clock, storeLag: 2 * time.Second, deliverLag: 5 * time.Second, blocked: map[int64]bool{}}
}

func (f *fakePERCo) apply() {
	keep := f.queued[:0]
	for _, q := range f.queued {
		if f.clock.Before(q.at) {
			keep = append(keep, q)
			continue
		}
		for _, id := range q.ids {
			f.blocked[id] = q.blocked
		}
	}
	f.queued = keep
}

func (f *fakePERCo) StaffBlockState(_ context.Context, id int64) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apply()
	return f.blocked[id], true, nil
}

func (f *fakePERCo) SetBlocked(ctx context.Context, id int64, blocked bool) error {
	return f.SetBlockedMany(ctx, []int64{id}, blocked, perco.IDsComma)
}

func (f *fakePERCo) SetBlockedMany(_ context.Context, ids []int64, blocked bool, format string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.failWrites {
		return errors.New("HTTP 500")
	}
	switch {
	case format == f.strangerOn && blocked:
		ids = []int64{1}
	case format != perco.IDsComma:
		ids = ids[:1] // PERCo took only the first id
	}
	f.queued = append(f.queued, queued{at: f.clock.Add(f.storeLag), ids: ids, blocked: blocked})
	f.pending += len(ids)
	f.pendingTo = f.clock.Add(f.storeLag + f.deliverLag)
	return nil
}

func (f *fakePERCo) DeviceHealth(context.Context, []int) (perco.DeviceHealth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.clock.Before(f.pendingTo) {
		f.pending = 0
	}
	return perco.DeviceHealth{Connected: !f.offline, Pending: f.pending}, nil
}

func (f *fakePERCo) Events(context.Context, time.Time, time.Time, int, int) ([]perco.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.clock = f.clock.Add(f.eventsLag)
	return nil, nil
}

func (f *fakePERCo) BlockedStaff(context.Context) ([]perco.BlockedPerson, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apply()
	var out []perco.BlockedPerson
	for id, b := range f.blocked {
		if b {
			out = append(out, perco.BlockedPerson{ID: id})
		}
	}
	return out, nil
}

func newBench(t *testing.T) (*bench, *fakePERCo, *strings.Builder) {
	clock := time.Date(2026, 10, 16, 15, 0, 0, 0, time.UTC)
	f := newFake(&clock)
	var sb strings.Builder
	b := &bench{api: f, devices: []int{2}, out: log.New(io.MultiWriter(&sb, io.Discard), "", 0),
		now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return clock },
		sleep: func(_ context.Context, d time.Duration) error {
			f.mu.Lock()
			clock = clock.Add(d)
			f.mu.Unlock()
			return nil
		},
		poll: time.Second}
	return b, f, &sb
}

var testIDs = []int64{960, 961, 962, 963, 964}

func TestParseIDsOnlyTestEmployees(t *testing.T) {
	if _, err := parseIDs("960,900", []int64{960}); err == nil {
		t.Fatal("a real employee must be refused")
	}
	if _, err := parseIDs("1,2,3,4,5,6,7,8,9,10,11", []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}); err == nil {
		t.Fatal("more than 10 ids must be refused")
	}
	ids, err := parseIDs(" 960, 961,960", []int64{960, 961})
	if err != nil || len(ids) != 2 {
		t.Fatalf("ids %v, err %v", ids, err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	b, f, _ := newBench(t)
	if code := b.runBlock(context.Background(), testIDs, 3, 20*time.Second, time.Minute, "auto", false); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if f.writes != 0 {
		t.Fatalf("dry run wrote %d times", f.writes)
	}
}

func TestMeasuresStagesAndUnblocks(t *testing.T) {
	b, f, out := newBench(t)
	if code := b.runBlock(context.Background(), testIDs, 2, 20*time.Second, time.Minute, "auto", true); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, id := range testIDs {
		if f.blocked[id] {
			t.Fatalf("%d left blocked\n%s", id, out)
		}
	}
	s := out.String()
	for _, want := range []string{"формат comma: PERCo применил ко всем 5", "ИТОГ блокировка пачкой из 5 (2 замеров)", "доставлено в контроллер 7.0 с"} {
		if !strings.Contains(s, want) {
			t.Fatalf("report lacks %q:\n%s", want, s)
		}
	}
}

func TestRefusesWhenEmployeeAlreadyBlocked(t *testing.T) {
	b, f, out := newBench(t)
	f.blocked[962] = true // an operator's block: not ours to touch
	if code := b.runBlock(context.Background(), testIDs, 1, time.Second, time.Minute, "comma", true); code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if f.writes != 0 || !f.blocked[962] {
		t.Fatal("nothing may be written, the operator's block stays")
	}
}

func TestRefusesWhenControllerOffline(t *testing.T) {
	b, f, _ := newBench(t)
	f.offline = true
	if code := b.runBlock(context.Background(), testIDs, 1, time.Second, time.Minute, "comma", true); code != 1 || f.writes != 0 {
		t.Fatalf("exit %d, writes %d", code, f.writes)
	}
}

func TestStrangerBlockedByWrongFormatStops(t *testing.T) {
	b, f, out := newBench(t)
	f.strangerOn = perco.IDsComma
	if code := b.runBlock(context.Background(), testIDs, 1, time.Second, time.Minute, "auto", true); code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out.String(), "посторонние id [1]") {
		t.Fatalf("the stranger must be reported:\n%s", out)
	}
}

func TestControllerLostMidwayStillUnblocks(t *testing.T) {
	b, f, out := newBench(t)
	f.deliverLag = time.Hour // queue never drains
	b.runBlock(context.Background(), testIDs, 1, time.Second, 30*time.Second, "comma", true)
	if !strings.Contains(out.String(), "очередь контроллера не опустела") {
		t.Fatalf("timeout must be reported:\n%s", out)
	}
	for _, id := range testIDs {
		if f.blocked[id] {
			t.Fatalf("%d left blocked after a failed round", id)
		}
	}
}

func TestLoadStopsWhenPERCoSlows(t *testing.T) {
	b, f, out := newBench(t)
	f.eventsLag = 4 * time.Second // slower than slowP95
	b.runLoad(context.Background(), "1,2,4", 10*time.Second, true)
	s := out.String()
	if !strings.Contains(s, "ОСТАНОВЛЕНО") || strings.Contains(s, "ступень 2 запр/с") {
		t.Fatalf("must stop at the first slow step:\n%s", s)
	}
}

func TestLoadCapsRate(t *testing.T) {
	b, _, out := newBench(t)
	if code := b.runLoad(context.Background(), "1,6", time.Second, true); code != 2 {
		t.Fatalf("6 req/s must be refused, exit %d\n%s", code, out)
	}
}

func TestBaselineFlagsHeavyEventQuery(t *testing.T) {
	b, f, out := newBench(t)
	f.eventsLag = 2 * time.Second // state answers instantly in the fake: any event lag is "heavy"
	b.baseline(context.Background())
	if !strings.Contains(out.String(), "запрос событий тяжёлый") {
		t.Fatalf("heavy event query must be flagged:\n%s", out)
	}
}
