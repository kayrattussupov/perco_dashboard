// Package engine polls PERCo for canteen events, keeps the local store in sync
// and builds the views served to the dashboard. Access rights are changed only by the
// Blocker, and only when the dashboard switch is not "off".
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"perco-meal/internal/config"
	"perco-meal/internal/meals"
	"perco-meal/internal/perco"
	"perco-meal/internal/store"
)

const (
	pageRows       = 500
	overlap        = 10 * time.Minute // re-read window to catch late controller events
	fullResyncEach = 10 * time.Minute
	staffWorkers   = 4
)

type Engine struct {
	cfg *config.Config
	pc  *perco.Client
	st  *store.Store
	log *log.Logger
	loc *time.Location

	mu      sync.RWMutex
	meals   []meals.Meal
	lastOK  time.Time
	lastErr string

	subsMu sync.Mutex
	subs   map[chan struct{}]struct{}

	fetching sync.Map // user ids being fetched on demand

	blocker *Blocker
}

func New(cfg *config.Config, pc *perco.Client, st *store.Store, lg *log.Logger) *Engine {
	e := &Engine{cfg: cfg, pc: pc, st: st, log: lg, loc: cfg.Location(), subs: map[chan struct{}]struct{}{}}
	e.blocker = &Blocker{cfg: cfg, api: pc, st: st, log: lg, loc: e.loc, now: e.Now, meals: e.Meals,
		healthy: e.healthy, notify: e.notify, self: cfg.Perco.Login}
	if raw := st.GetKV("meals"); raw != "" {
		var ms []meals.Meal
		if json.Unmarshal([]byte(raw), &ms) == nil {
			e.meals = ms
		}
	}
	if err := st.MarkDeny(append(slices.Clone(builtinDenyIDs), cfg.DenyEventIDs...)); err != nil {
		lg.Printf("переклассификация отказов: %v", err)
	}
	return e
}

func (e *Engine) Now() time.Time { return time.Now().In(e.loc) }

func (e *Engine) Photo(id int64) (string, error) { return e.st.Photo(id) }

func (e *Engine) Blocker() *Blocker { return e.blocker }

// healthy means the PERCo link is up and meal windows are known — the precondition for blocking.
func (e *Engine) healthy() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastErr == "" && len(e.meals) > 0 && time.Since(e.lastOK) < 30*time.Second
}

func (e *Engine) Meals() []meals.Meal {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.meals
}

// Run blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	go e.every(ctx, time.Duration(e.cfg.StaffRefreshMinutes)*time.Minute, "сотрудники", e.syncStaff)
	go e.every(ctx, 10*time.Minute, "окна питания", e.loadMeals)
	go e.every(ctx, 15*time.Second, "блокировки", e.blocker.Tick) // first run at start: recover after downtime
	if e.cfg.BackupReset {
		go e.every(ctx, 20*time.Second, "резервный сброс", e.backupReset)
	}

	tick := time.NewTicker(time.Duration(e.cfg.PollSeconds) * time.Second)
	defer tick.Stop()
	var lastFull time.Time
	for {
		full := time.Since(lastFull) > fullResyncEach
		if err := e.poll(ctx, full); err != nil {
			e.setErr(err)
		} else {
			e.setOK()
			if full {
				lastFull = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (e *Engine) every(ctx context.Context, d time.Duration, what string, fn func(context.Context) error) {
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			e.log.Printf("%s: %v", what, err)
			d := min(d, time.Minute) // retry sooner after a failure
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
}

func (e *Engine) setOK() {
	e.mu.Lock()
	if e.lastErr != "" {
		e.log.Printf("связь с PERCo восстановлена")
	}
	e.lastOK, e.lastErr = time.Now(), ""
	e.mu.Unlock()
}

func (e *Engine) setErr(err error) {
	e.mu.Lock()
	if e.lastErr != err.Error() {
		e.log.Printf("опрос событий: %v", err)
	}
	e.lastErr = err.Error()
	e.mu.Unlock()
}

func (e *Engine) poll(ctx context.Context, full bool) error {
	now := e.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, e.loc)
	var from time.Time
	if last := e.st.GetKV("last_ts"); last == "" {
		// First start: import the current month so reports are not empty.
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, e.loc)
	} else if full {
		from = midnight
	} else {
		t, err := time.ParseInLocation(perco.TimeLayout, last, e.loc)
		if err != nil {
			t = midnight
		}
		from = t.Add(-overlap)
	}
	to := now.Add(time.Hour)

	changed := false
	maxTS := e.st.GetKV("last_ts")
	for page := 1; ; page++ {
		rows, err := e.pc.Events(ctx, from, to, page, pageRows)
		if err != nil {
			return err
		}
		for _, ev := range rows {
			isNew, err := e.handle(ctx, ev)
			if err != nil {
				return err
			}
			changed = changed || isNew
			if ev.TimeLabel > maxTS && ev.TimeLabel <= to.Format(perco.TimeLayout) {
				maxTS = ev.TimeLabel
			}
		}
		if len(rows) < pageRows {
			break
		}
	}
	if maxTS != "" {
		e.st.SetKV("last_ts", maxTS)
	}
	if changed {
		e.notify()
	}
	return nil
}

// builtinDenyIDs are controller refusals. With "Жёсткая" time/antipass control and the lock
// algorithm "Регистрация прохода по предъявлению идентификатора", the CT/L14 logs a refusal as
// "Проход по идентификатору: с несоответствием …" (529/273/785) while the lock stays shut
// (verified on site 2026-10-08).
var builtinDenyIDs = []int{
	8,   // Запрет прохода
	264, // Запрет прохода: несоответствие текущему местоположению
	273, // Проход по идентификатору: с несоответствием текущему местоположению (antipass)
	520, // Запрет прохода: несоответствие режиму доступа
	529, // Проход по идентификатору: с несоответствием временным критериям доступа
	776, // Запрет прохода: несоответствие временным критериям доступа и текущему местоположению
	785, // Проход по идентификатору: несоответствие временным критериям доступа и текущему местоположению
}

// resetEventID is "Очистить пространственные зоны для идентификаторов: Выполнено", logged on the
// controller (not the lock) when "Сбросить зональность" runs (verified on site 2026-10-08).
const resetEventID = 16055

func (e *Engine) classify(ev perco.Event) string {
	c := e.cfg
	if ev.EventNameID == resetEventID || slices.Contains(c.ResetMonitor.EventIDs, ev.EventNameID) {
		return store.KindReset
	}
	if ev.EventNameID == evStaffBlocked || ev.EventNameID == evStaffUnblocked {
		return store.KindStaffOp
	}
	if ev.DeviceID == nil || !slices.Contains(c.DeviceIDs, *ev.DeviceID) {
		return ""
	}
	name := strings.ToLower(ev.EventName)
	switch {
	case slices.Contains(c.PassEventIDs, ev.EventNameID):
		return store.KindPass
	case slices.Contains(builtinDenyIDs, ev.EventNameID),
		slices.Contains(c.DenyEventIDs, ev.EventNameID),
		strings.Contains(name, "запрет"),
		strings.Contains(name, "несоответств"):
		return store.KindDeny
	case slices.Contains(c.DoorEventIDs, ev.EventNameID):
		return store.KindDoor
	}
	return store.KindOther
}

func (e *Engine) handle(ctx context.Context, ev perco.Event) (bool, error) {
	kind := e.classify(ev)
	if kind == "" {
		return false, nil
	}
	t, err := time.ParseInLocation(perco.TimeLayout, ev.TimeLabel, e.loc)
	if err != nil {
		return false, nil
	}
	se := store.Event{
		ID: ev.ID, TS: ev.TimeLabel, Day: t.Format("2006-01-02"), Kind: kind,
		Code: ev.EventNameID, Name: strings.TrimSpace(strings.TrimRight(strings.TrimSpace(ev.EventName), ":")),
	}
	if ev.UserID != nil {
		se.UserID = *ev.UserID
	}
	se.Meal = meals.AtFor(e.Meals(), t, e.st.StaffTemplates(se.UserID))
	if ev.FIO != nil {
		se.FIO = strings.Join(strings.Fields(*ev.FIO), " ")
	}
	if ev.DeviceID != nil {
		se.DeviceID = *ev.DeviceID
	}
	if ev.UserName != nil {
		se.Actor = *ev.UserName
	}
	isNew, err := e.st.InsertEvent(se)
	if err != nil || !isNew {
		return isNew, err
	}
	if se.UserID != 0 && !e.st.StaffKnown(se.UserID) {
		go e.fetchStaff(ctx, se.UserID, se.FIO)
	}
	if kind == store.KindPass {
		e.blocker.OnPass(ctx, se, t)
	}
	return true, nil
}

func (e *Engine) loadMeals(ctx context.Context) error {
	var ms []meals.Meal
	for _, cm := range e.cfg.Meals {
		var ivs []meals.Interval
		sids, err := e.pc.TemplateSchedules(ctx, cm.TemplateID)
		if err != nil {
			return fmt.Errorf("шаблон %d: %w", cm.TemplateID, err)
		}
		for _, sid := range sids {
			got, err := e.pc.ScheduleIntervals(ctx, sid)
			if err != nil {
				return fmt.Errorf("расписание %d: %w", sid, err)
			}
			for _, iv := range got {
				ivs = append(ivs, meals.Interval{Begin: iv.Begin, End: iv.End})
			}
		}
		if len(ivs) == 0 {
			continue
		}
		ms = append(ms, meals.Expand(cm.Key, cm.Name, cm.TemplateID, ivs)...)
	}
	raw, _ := json.Marshal(ms)
	e.mu.Lock()
	changed := !slices.EqualFunc(ms, e.meals, func(a, b meals.Meal) bool {
		return a.Key == b.Key && slices.Equal(a.Intervals, b.Intervals)
	})
	e.meals = ms
	e.mu.Unlock()
	if changed {
		e.st.SetKV("meals", string(raw))
		for _, m := range ms {
			if b, en, ok := m.Span(); ok {
				e.log.Printf("окно %s: %s–%s (шаблон %d)", m.Name, meals.FmtSec(b), meals.FmtSec(en), m.TemplateID)
			}
		}
		e.notify()
	}
	return nil
}

func (e *Engine) syncStaff(ctx context.Context) error {
	list, err := e.pc.StaffList(ctx)
	if err != nil {
		return err
	}
	jobs := make(chan perco.StaffBrief)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ids []int64
	var firstErr error
	for range staffWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				d, err := e.pc.Staff(ctx, b.ID)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					continue
				}
				ids = append(ids, b.ID)
				mu.Unlock()
				st := store.Staff{ID: b.ID, FIO: b.FIO(), Division: deref(b.DivisionName),
					Position: deref(b.PositionName), Tabel: deref(b.TabelNumber), Templates: d.TemplateIDs()}
				if err := e.st.UpsertStaff(st, deref(d.Photo)); err != nil {
					e.log.Printf("сохранение сотрудника %d: %v", b.ID, err)
				}
			}
		}()
	}
	for _, b := range list {
		if b.IsActive != 1 {
			continue
		}
		select {
		case jobs <- b:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr // keep previous active flags rather than deactivate on a partial read
	}
	if err := e.st.DeactivateExcept(ids); err != nil {
		return err
	}
	e.log.Printf("сотрудники обновлены: %d активных", len(ids))
	e.notify()
	return nil
}

func (e *Engine) fetchStaff(ctx context.Context, id int64, fio string) {
	if _, busy := e.fetching.LoadOrStore(id, true); busy {
		return
	}
	defer e.fetching.Delete(id)
	d, err := e.pc.Staff(ctx, id)
	if err != nil {
		return
	}
	if err := e.st.UpsertStaff(store.Staff{ID: id, FIO: fio, Templates: d.TemplateIDs()}, deref(d.Photo)); err == nil {
		e.notify()
	}
}

// backupReset sends "Сброс зональности" shortly before a meal window if PERCo's own task has not.
func (e *Engine) backupReset(ctx context.Context) error {
	now := e.Now()
	for _, m := range e.Meals() {
		begin, _, ok := m.Span()
		if !ok {
			continue
		}
		start := dayStart(now).Add(time.Duration(begin) * time.Second)
		if now.Before(start.Add(-2*time.Minute)) || !now.Before(start) {
			continue
		}
		if e.resetSeen(start, now) {
			continue
		}
		key := "backup_reset:" + start.Format(perco.TimeLayout)
		if e.st.GetKV(key) != "" {
			continue
		}
		for _, dev := range e.cfg.DeviceIDs {
			if err := e.pc.ResetAntipass(ctx, dev); err != nil {
				return fmt.Errorf("устройство %d: %w", dev, err)
			}
		}
		e.st.SetKV(key, now.Format(perco.TimeLayout))
		e.log.Printf("резервный сброс зональности перед «%s» отправлен", m.Name)
	}
	return nil
}

func (e *Engine) resetSeen(windowStart, until time.Time) bool {
	from := windowStart.Add(-time.Duration(e.cfg.ResetMonitor.LookbackMinutes) * time.Minute)
	ok, _ := e.st.HasKindBetween(store.KindReset, from.Format(perco.TimeLayout), until.Format(perco.TimeLayout))
	return ok
}

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// Subscribe returns a channel signalled whenever dashboard data changes.
func (e *Engine) Subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	e.subsMu.Lock()
	e.subs[ch] = struct{}{}
	e.subsMu.Unlock()
	return ch
}

func (e *Engine) Unsubscribe(ch chan struct{}) {
	e.subsMu.Lock()
	delete(e.subs, ch)
	e.subsMu.Unlock()
}

func (e *Engine) notify() {
	e.subsMu.Lock()
	defer e.subsMu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
