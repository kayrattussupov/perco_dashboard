package engine

import (
	"context"
	"crypto/subtle"
	"errors"
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

// Blocking modes, switched from the dashboard. "off" is the old way: dashboard only, PERCo untouched.
const (
	ModeOff  = "off"
	ModeTest = "test"
	ModeOn   = "on"
)

const (
	modeKey     = "blocking_mode"
	releaseLag  = time.Minute      // keep the block a little past the window end
	releaseLead = 30 * time.Minute // unblock this long before the person's next window starts
	maxPassAge  = 2 * time.Minute  // older passes are catch-up after downtime: never block on them
	maxClockGap = 2 * time.Minute  // controller vs. this PC clock; beyond it we do not trust our timing
	settleWarn  = 5 * time.Minute  // a change PERCo has not delivered to the controller by now is reported
)

const settleKey = "settle_since" // time of the oldest PERCo write not yet confirmed as delivered to the controller

// PERCo operator actions on an employee (verified on site 2026-10-12).
const (
	evStaffBlocked   = 501404 // Блокировка сотрудника
	evStaffUnblocked = 501405 // Снятие блокировки сотрудника
)

var retryDelays = []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute}

type blockAPI interface {
	StaffBlockState(ctx context.Context, id int64) (blocked, active bool, err error)
	SetBlocked(ctx context.Context, id int64, blocked bool) error
	DeviceHealth(ctx context.Context, deviceIDs []int) (perco.DeviceHealth, error)
	BlockedStaff(ctx context.Context) ([]perco.BlockedPerson, error)
}

// Blocker enforces one pass per meal window by blocking an employee in PERCo after the first
// pass and unblocking them when the window ends. Principles:
//   - fail open: any doubt (no link, no meal windows, stale event, clock skew) means no block;
//   - write-ahead: a block row exists before PERCo is touched, so a crash is recovered on start;
//   - operator wins: blocks/unblocks done by people in PERCo are never overridden;
//   - a block is held after the window until the next window of the employee today; with none left it ends
//     right after the window, so nothing stays blocked overnight (time control refuses outside windows anyway);
//   - every service block is released by that time, by max_hours, on start, or by "off".
type Blocker struct {
	cfg     *config.Config
	api     blockAPI
	st      *store.Store
	log     *log.Logger
	loc     *time.Location
	now     func() time.Time
	meals   func() []meals.Meal
	healthy func() bool
	notify  func()
	self    string // PERCo login of this service; its own block/unblock events are not "operator" actions

	mu     sync.Mutex // serializes block-row transitions and PERCo writes
	hmu    sync.Mutex // guards health; read by the dashboard without b.mu
	health     healthState
	blockedNow blockedSnapshot
}

func (b *Blocker) nowTS() string { return b.now().Format(perco.TimeLayout) }

func (b *Blocker) parse(ts string) (time.Time, error) {
	return time.ParseInLocation(perco.TimeLayout, ts, b.loc)
}

// Mode is the mode chosen on the dashboard.
func (b *Blocker) Mode() string {
	switch m := b.st.GetKV(modeKey); m {
	case ModeTest, ModeOn:
		return m
	}
	return ModeOff
}

// EffectiveMode applies the config kill switch.
func (b *Blocker) EffectiveMode() string {
	if b.cfg.Blocking.ForceOff {
		return ModeOff
	}
	return b.Mode()
}

func (b *Blocker) PINSet() bool { return b.cfg.Blocking.AdminPIN != "" }

// CheckPIN compares in constant time; an empty configured PIN disables all admin actions.
func (b *Blocker) CheckPIN(pin string) bool {
	want := b.cfg.Blocking.AdminPIN
	return want != "" && subtle.ConstantTimeCompare([]byte(pin), []byte(want)) == 1
}

// SetMode switches the mode; switching off releases every block the service owns.
func (b *Blocker) SetMode(ctx context.Context, mode, actor string) (released, failed int, err error) {
	if mode != ModeOff && mode != ModeTest && mode != ModeOn {
		return 0, 0, fmt.Errorf("неизвестный режим %q", mode)
	}
	if err := b.st.SetKV(modeKey, mode); err != nil {
		return 0, 0, err
	}
	b.st.Audit(b.nowTS(), actor, "mode", mode)
	b.log.Printf("режим блокировки: %s (%s)", mode, actor)
	if mode == ModeOff {
		released, failed = b.ReleaseAll(ctx, actor, "режим ВЫКЛ")
	}
	b.notify()
	return released, failed, nil
}

func (b *Blocker) close(r *store.Block, state, note string) {
	r.State, r.Note, r.ClosedAt, r.LastError, r.NextTry = state, note, b.nowTS(), "", ""
	if err := b.st.UpdateBlock(*r); err != nil {
		b.log.Printf("блокировка %d: %v", r.ID, err)
	}
}

// outPrefix marks blocks taken for a pass outside every window of the person; the rest of the key is the
// start of their next window ("out:11:30"), so each gap blocks once a day. With no window left today there is
// nothing to hold the person for and no block is taken.
const outPrefix = "out:"

// restoreFor returns when a block taken at `at` is released, and the block's key. In a meal window the block is
// held until 30 minutes before the person's next window (never before this window ends); outside windows, until
// 30 minutes before the next one. With no later window today a block taken in a window ends right after it
// (never held overnight, when the service or PERCo may be down) and a pass outside windows is not blocked.
func (b *Blocker) restoreFor(mealKey string, userID int64, at time.Time) (time.Time, string, bool) {
	day := dayStart(at)
	from, floor := meals.SecOfDay(at), time.Time{}
	key := mealKey
	if strings.HasPrefix(mealKey, outPrefix) || mealKey == "" {
		key = ""
	} else {
		m, ok := meals.Find(b.meals(), mealKey)
		if !ok {
			return time.Time{}, "", false
		}
		end, ok := m.EndAt(at)
		if !ok {
			return time.Time{}, "", false
		}
		from, floor = end, day.Add(time.Duration(end)*time.Second+releaseLag)
	}
	var restore time.Time
	next := 0
	if begin, ok := meals.NextBegin(b.meals(), from, b.st.StaffTemplates(userID)); ok {
		restore, next = day.Add(time.Duration(begin)*time.Second-releaseLead), begin
	} else if floor.IsZero() {
		return time.Time{}, "", false // outside windows and none left today: nothing to hold them for
	} else {
		restore = floor // last window of the day: release right after it
	}
	if restore.Before(floor) {
		restore = floor
	}
	if key == "" {
		key = outPrefix + meals.FmtSec(next)
	}
	if strings.HasPrefix(mealKey, outPrefix) {
		key = mealKey
	}
	return restore, key, true
}

// OnPass is called for every newly seen pass. It blocks the employee if this is their first pass
// in the current meal window and every safety condition holds.
func (b *Blocker) OnPass(ctx context.Context, ev store.Event, passAt time.Time) {
	mode := b.EffectiveMode()
	if mode == ModeOff || ev.UserID == 0 {
		return
	}
	if mode == ModeTest && !slices.Contains(b.cfg.Blocking.TestUserIDs, ev.UserID) {
		return
	}
	now := b.now()
	if gap := now.Sub(passAt); gap > maxPassAge || gap < -maxClockGap {
		if gap < -maxClockGap {
			b.log.Printf("блокировка пропущена: время события %s опережает часы этого ПК — проверьте время", ev.TS)
		}
		return
	}
	if !b.healthy() {
		return
	}
	restore, key, ok := b.restoreFor(ev.Meal, ev.UserID, passAt)
	if !ok {
		return
	}
	deadline := now.Add(time.Duration(b.cfg.Blocking.MaxHours * float64(time.Hour)))
	if !now.Before(restore) {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	r := store.Block{UserID: ev.UserID, FIO: ev.FIO, Day: ev.Day, Meal: key, State: store.BlockPending,
		PassEventID: ev.ID, CreatedAt: b.nowTS(), RestoreAt: restore.Format(perco.TimeLayout),
		Deadline: deadline.Format(perco.TimeLayout)}
	id, err := b.st.CreateBlock(r)
	if errors.Is(err, store.ErrBlockExists) {
		return // already handled this window (also: an operator unblocked them — do not block again)
	}
	if err != nil {
		b.log.Printf("блокировка %s: %v", ev.FIO, err)
		return
	}
	r.ID = id

	if !b.linkUp(ctx) {
		b.close(&r, store.BlockClosed, "не заблокирован: контроллер не на связи с PERCo")
		return
	}
	blocked, active, err := b.api.StaffBlockState(ctx, ev.UserID)
	switch {
	case err != nil:
		b.close(&r, store.BlockClosed, "не заблокирован: нет связи с PERCo")
		return
	case !active:
		b.close(&r, store.BlockClosed, "не заблокирован: сотрудник неактивен")
		return
	case blocked:
		b.close(&r, store.BlockClosed, "не заблокирован: уже заблокирован оператором")
		return
	}
	if err := b.api.SetBlocked(ctx, ev.UserID, true); err != nil {
		// Unknown whether PERCo applied it; Tick reconciles the pending row against PERCo.
		r.LastError = err.Error()
		b.st.UpdateBlock(r)
		b.log.Printf("блокировка %s: %v", ev.FIO, err)
		return
	}
	if now, _, err := b.api.StaffBlockState(ctx, ev.UserID); err == nil && !now {
		// PERCo answered "ok" but the flag is not there: leave the row pending, Tick closes it (fail open).
		r.LastError = "PERCo не подтвердил блокировку"
		b.st.UpdateBlock(r)
		b.log.Printf("блокировка %s: %s", ev.FIO, r.LastError)
		return
	}
	r.State, r.BlockedAt = store.BlockActive, b.nowTS()
	b.st.UpdateBlock(r)
	b.markUnsettled()
	b.st.Audit(r.BlockedAt, "service", "block", fmt.Sprintf("%d %s %s до %s", r.UserID, r.FIO, r.Meal, r.RestoreAt[11:16]))
	b.log.Printf("заблокирован %s (%s) до %s", r.FIO, r.Meal, r.RestoreAt[11:16])
	b.notify()
}

// Tick reconciles open rows with PERCo and releases due blocks. Runs every 15 s and at start.
func (b *Blocker) Tick(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	open, err := b.st.OpenBlocks()
	if err != nil {
		return err
	}
	changed := false
	for i := range open {
		if ctx.Err() != nil {
			return nil
		}
		r := &open[i]
		before := *r
		switch r.State {
		case store.BlockPending:
			b.reconcilePending(ctx, r)
		case store.BlockActive:
			b.tickActive(ctx, r)
		case store.BlockHeld:
			if blocked, _, err := b.api.StaffBlockState(ctx, r.UserID); err == nil && !blocked {
				b.close(r, store.BlockClosed, "оператор снял свою блокировку")
			}
		}
		changed = changed || *r != before
	}
	b.checkSettled(ctx)
	if changed {
		b.notify()
	}
	return nil
}

// markUnsettled remembers that PERCo was written to and the controller has not yet confirmed receipt.
func (b *Blocker) markUnsettled() {
	if b.st.GetKV(settleKey) == "" {
		b.st.SetKV(settleKey, b.nowTS())
	}
}

// checkSettled clears the "unsettled" mark once PERCo has nothing left to deliver to the controller.
// Caller holds b.mu.
func (b *Blocker) checkSettled(ctx context.Context) {
	since := b.st.GetKV(settleKey)
	if since == "" {
		return
	}
	h, err := b.api.DeviceHealth(ctx, b.cfg.DeviceIDs)
	b.noteHealth(h, err)
	if err == nil && h.Settled() {
		b.st.SetKV(settleKey, "")
		b.log.Printf("изменения доставлены в контроллер (ожидали с %s)", since[11:])
	}
}

// reconcilePending resolves a row left between "intent" and "PERCo confirmed" (crash or error).
func (b *Blocker) reconcilePending(ctx context.Context, r *store.Block) {
	blocked, _, err := b.api.StaffBlockState(ctx, r.UserID)
	if err != nil {
		return // try again next tick
	}
	if !blocked {
		b.close(r, store.BlockClosed, "блокировка не применилась")
		return
	}
	if ops, _ := b.st.OperatorActions(r.UserID, r.CreatedAt, b.self); len(ops) > 0 && ops[len(ops)-1].Code == evStaffBlocked {
		r.State, r.Note = store.BlockHeld, "заблокирован оператором "+ops[len(ops)-1].Actor
		b.st.UpdateBlock(*r)
		return
	}
	r.State, r.BlockedAt, r.LastError = store.BlockActive, b.nowTS(), ""
	b.st.UpdateBlock(*r)
}

func (b *Blocker) tickActive(ctx context.Context, r *store.Block) {
	// Operator actions after our block decide first: their word is final.
	if ops, err := b.st.OperatorActions(r.UserID, r.BlockedAt, b.self); err == nil && len(ops) > 0 {
		last := ops[len(ops)-1]
		if last.Code == evStaffUnblocked {
			b.close(r, store.BlockClosed, "разблокирован оператором "+last.Actor)
		} else {
			r.State, r.Note = store.BlockHeld, "заблокирован оператором "+last.Actor
			b.st.UpdateBlock(*r)
		}
		return
	}
	b.followWindow(r)

	now := b.now()
	restore, err1 := b.parse(r.RestoreAt)
	deadline, err2 := b.parse(r.Deadline)
	due := err1 != nil || err2 != nil || !now.Before(restore) || !now.Before(deadline) || r.Day < now.Format("2006-01-02")
	if !due {
		return
	}
	if r.NextTry != "" {
		if next, err := b.parse(r.NextTry); err == nil && now.Before(next) {
			return
		}
	}
	b.release(ctx, r, "окно закончилось", false)
}

// followWindow moves the release time when an operator edits the meal window (never past the deadline).
func (b *Blocker) followWindow(r *store.Block) {
	created, err := b.parse(r.CreatedAt)
	if err != nil {
		return
	}
	restore, _, ok := b.restoreFor(r.Meal, r.UserID, created)
	if !ok {
		return
	}
	if deadline, err := b.parse(r.Deadline); err == nil && restore.After(deadline) {
		restore = deadline
	}
	if ts := restore.Format(perco.TimeLayout); ts != r.RestoreAt {
		r.RestoreAt = ts
		b.st.UpdateBlock(*r)
	}
}

// release unblocks a row the service owns. Caller holds b.mu. Unless force is set (an operator's explicit
// order), nothing is written while PERCo is not connected to the controller; the attempt is retried later.
// A write counts only after PERCo reports the flag cleared; delivery to the controller is confirmed by Tick.
func (b *Blocker) release(ctx context.Context, r *store.Block, why string, force bool) bool {
	var err error
	if !force && !b.linkUp(ctx) {
		err = errors.New("контроллер не на связи с PERCo — ждём")
	}
	var blocked, active bool
	if err == nil {
		blocked, active, err = b.api.StaffBlockState(ctx, r.UserID)
	}
	if err == nil {
		switch {
		case !blocked:
			b.close(r, store.BlockClosed, "уже разблокирован")
			return true
		case !active:
			b.close(r, store.BlockClosed, "сотрудник неактивен — блокировку не снимаем")
			return true
		}
		if err = b.api.SetBlocked(ctx, r.UserID, false); err == nil {
			var still bool
			if still, _, err = b.api.StaffBlockState(ctx, r.UserID); err == nil && still {
				err = errors.New("PERCo не подтвердил снятие блокировки")
			}
		}
	}
	if err != nil {
		r.Attempts++
		r.LastError = err.Error()
		r.NextTry = b.now().Add(retryDelays[min(r.Attempts, len(retryDelays))-1]).Format(perco.TimeLayout)
		b.st.UpdateBlock(*r)
		b.log.Printf("не удалось разблокировать %s (попытка %d): %v", r.FIO, r.Attempts, err)
		return false
	}
	b.close(r, store.BlockReleased, why)
	b.markUnsettled()
	b.st.Audit(r.ClosedAt, "service", "unblock", fmt.Sprintf("%d %s: %s", r.UserID, r.FIO, why))
	b.log.Printf("разблокирован %s: %s", r.FIO, why)
	return true
}

// ReleaseAll unblocks everything the service owns now (operator-held rows are left alone).
func (b *Blocker) ReleaseAll(ctx context.Context, actor, why string) (released, failed int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	open, err := b.st.OpenBlocks()
	if err != nil {
		b.log.Printf("снять все блокировки: %v", err)
		return 0, 0
	}
	for i := range open {
		r := &open[i]
		if r.State == store.BlockPending {
			b.reconcilePending(ctx, r)
		}
		if r.State != store.BlockActive {
			continue
		}
		if b.release(ctx, r, why+" ("+actor+")", true) {
			released++
		} else {
			failed++
		}
	}
	b.st.Audit(b.nowTS(), actor, "release-all", fmt.Sprintf("%s: снято %d, ошибок %d", why, released, failed))
	b.notify()
	return released, failed
}

// ReleaseOne is a manual unblock from the dashboard; a person decides, so operator-held rows are released too.
func (b *Blocker) ReleaseOne(ctx context.Context, id int64, actor string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, err := b.st.BlockByID(id)
	if err != nil {
		return err
	}
	if r.State != store.BlockActive && r.State != store.BlockHeld && r.State != store.BlockPending {
		return errors.New("блокировка уже закрыта")
	}
	ok := b.release(ctx, &r, "снято вручную ("+actor+")", true)
	b.notify()
	if !ok {
		return errors.New(r.LastError)
	}
	return nil
}

type BlockingStatus struct {
	Mode      string        `json:"mode"`
	Effective string        `json:"effective"`
	ForceOff  bool          `json:"force_off"`
	PINSet    bool          `json:"pin_set"`
	Warnings  []string      `json:"warnings"`
	Open      []store.Block `json:"open"`
	Today     []store.Block `json:"today"`
	Checks    []CheckRecord `json:"checks"`
	// Controller is the live state of the PERCo-Web ↔ controller link, shown in the dashboard header.
	Controller ControllerStatus `json:"controller"`
	// Blocked is everyone blocked in PERCo at this moment, whoever blocked them.
	Blocked BlockedNow `json:"blocked_now"`
}

func (b *Blocker) Status() (*BlockingStatus, error) {
	open, err := b.st.OpenBlocks()
	if err != nil {
		return nil, err
	}
	today, err := b.st.BlocksOfDay(b.now().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	s := &BlockingStatus{Mode: b.Mode(), Effective: b.EffectiveMode(), ForceOff: b.cfg.Blocking.ForceOff,
		PINSet: b.cfg.Blocking.AdminPIN != "", Warnings: []string{}, Open: open, Today: today}
	if s.Open == nil {
		s.Open = []store.Block{}
	}
	if s.Today == nil {
		s.Today = []store.Block{}
	}
	failing, held := 0, 0
	for _, r := range open {
		if r.Attempts > 0 {
			failing++
		}
		if r.State == store.BlockHeld {
			held++
		}
	}
	if failing > 0 {
		s.Warnings = append(s.Warnings, fmt.Sprintf("Не удаётся снять блокировку: %d чел. Служба повторяет попытки; при необходимости разблокируйте в PERCo.", failing))
	}
	if held > 0 {
		s.Warnings = append(s.Warnings, fmt.Sprintf("Заблокированы оператором поверх блокировки службы: %d чел. Служба их не разблокирует.", held))
	}
	s.Checks = []CheckRecord{}
	if b.cfg.SelfCheck.Enabled {
		for _, hm := range b.cfg.SelfCheck.Times {
			rec, ok := b.CheckResult(hm)
			if !ok {
				continue
			}
			s.Checks = append(s.Checks, rec)
		}
		// Only the latest run speaks: a later clean check supersedes an earlier failed one.
		if last := latestCheck(s.Checks); last != nil && !last.OK {
			if t, err := b.parse(last.RanAt); err == nil && b.now().Sub(t) < 26*time.Hour {
				s.Warnings = append(s.Warnings, fmt.Sprintf("Проверка %s (%s): %s", last.Time, last.RanAt[:16], strings.Join(last.Problems, "; ")))
			}
		}
	}
	if since := b.st.GetKV(settleKey); since != "" {
		if t, err := b.parse(since); err == nil && b.now().Sub(t) > settleWarn {
			why := b.Controller().Text
			s.Warnings = append(s.Warnings, fmt.Sprintf("Изменения блокировок не подтверждены контроллером с %s. %s.", since[11:16], why))
		}
	}
	s.Controller = b.Controller()
	s.Blocked = b.BlockedNowStatus(open)
	if s.Controller.State == StateOffline {
		s.Warnings = append(s.Warnings, fmt.Sprintf("Контроллер не на связи с PERCo-Web с %s: проходы по картам и блокировки могут не работать.%s",
			s.Controller.Since[11:16], withMsg(s.Controller.Message)))
	}
	if s.Effective != ModeOff && b.self == "admin" {
		s.Warnings = append(s.Warnings, "Служба работает под оператором admin: блокировки, сделанные людьми под admin, будут считаться блокировками службы. Заведите отдельного оператора (например, service) и укажите его в config.yaml.")
	}
	return s, nil
}

// latestCheck returns the check that ran most recently.
func latestCheck(recs []CheckRecord) *CheckRecord {
	var last *CheckRecord
	for i := range recs {
		if last == nil || recs[i].RanAt > last.RanAt {
			last = &recs[i]
		}
	}
	return last
}
