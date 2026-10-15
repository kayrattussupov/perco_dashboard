package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"perco-meal/internal/store"
)

const (
	checkKeyPrefix = "selfcheck:"      // kv: last result per configured time, e.g. "selfcheck:06:30"
	checkGrace     = 30 * time.Minute  // how long a due check waits for the controller before it reports the outage
	checkWorkers   = 8                 // parallel employee reads during a check
	maxNames       = 8                 // employees named in one problem line
)

// CheckRecord is the stored outcome of one scheduled check.
type CheckRecord struct {
	Time     string   `json:"time"` // configured time of day, "06:30"
	Due      string   `json:"due"`  // the moment it was due, "2026-10-15 06:30:00"
	RanAt    string   `json:"ran_at"`
	Late     bool     `json:"late"` // ran more than 10 minutes after Due (the PC or the service was off)
	OK       bool     `json:"ok"`
	Problems []string `json:"problems"`
}

// SelfCheckTick runs every scheduled check that is due and has not run for today's moment yet. A check missed
// because the PC was off runs as soon as the service is back (marked Late). Cheap when nothing is due.
func (b *Blocker) SelfCheckTick(ctx context.Context) error {
	if !b.cfg.SelfCheck.Enabled {
		return nil
	}
	now := b.now()
	for _, hm := range b.cfg.SelfCheck.Times {
		due, ok := b.dueToday(hm, now)
		if !ok || now.Before(due) {
			continue
		}
		if rec, ok := b.CheckResult(hm); ok && rec.Due == due.Format("2006-01-02 15:04:05") {
			continue
		}
		b.runCheck(ctx, hm, due)
		if ctx.Err() != nil {
			return nil
		}
	}
	return nil
}

func (b *Blocker) dueToday(hm string, now time.Time) (time.Time, bool) {
	t, err := time.Parse("15:04", hm)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, b.loc), true
}

// CheckResult returns the last stored result of the check configured at hm.
func (b *Blocker) CheckResult(hm string) (CheckRecord, bool) {
	var rec CheckRecord
	raw := b.st.GetKV(checkKeyPrefix + hm)
	if raw == "" || json.Unmarshal([]byte(raw), &rec) != nil {
		return CheckRecord{}, false
	}
	return rec, true
}

// runCheck performs one check. While the controller is not ready it waits (returns without a result) for
// up to checkGrace after the due time, then records the outage as the problem.
func (b *Blocker) runCheck(ctx context.Context, hm string, due time.Time) {
	now := b.now()
	h, herr := b.api.DeviceHealth(ctx, b.cfg.DeviceIDs)
	b.noteHealth(h, herr)
	ready := herr == nil && h.Settled()
	if !ready && now.Sub(due) < checkGrace {
		return
	}

	var problems []string
	switch {
	case herr != nil:
		problems = append(problems, "Нет связи с PERCo-Web: "+herr.Error())
	case !h.Connected:
		problems = append(problems, "Контроллер не на связи с PERCo-Web"+withMsg(h.Message))
	case h.Pending > 0:
		problems = append(problems, fmt.Sprintf("В контроллер ещё не доставлено команд: %d", h.Pending))
	}
	if herr == nil && len(h.Errors) > 0 {
		problems = append(problems, "Не переданы в контроллер: "+strings.Join(h.Errors, ", "))
	}

	if herr == nil && h.Connected {
		_ = b.Tick(ctx) // release everything that is due first
		problems = append(problems, b.overdueBlocks()...)
		problems = append(problems, b.strayBlocks(ctx)...)
		problems = append(problems, b.unknownCards(now)...)
	}

	rec := CheckRecord{Time: hm, Due: due.Format("2006-01-02 15:04:05"), RanAt: b.nowTS(),
		Late: now.Sub(due) > 10*time.Minute, OK: len(problems) == 0, Problems: problems}
	if rec.Problems == nil {
		rec.Problems = []string{}
	}
	raw, _ := json.Marshal(rec)
	b.st.SetKV(checkKeyPrefix+hm, string(raw))

	verdict := "ОК"
	if !rec.OK {
		verdict = "ПРОБЛЕМА: " + strings.Join(problems, "; ")
	}
	if rec.Late {
		verdict += " (выполнена с опозданием)"
	}
	b.st.Audit(rec.RanAt, "service", "selfcheck", hm+" "+verdict)
	b.log.Printf("проверка %s: %s", hm, verdict)
	b.notify()
}

func withMsg(m string) string {
	if m == "" {
		return ""
	}
	return " (" + m + ")"
}

// overdueBlocks lists the service's own blocks that should already be gone but are not.
func (b *Blocker) overdueBlocks() []string {
	open, err := b.st.OpenBlocks()
	if err != nil {
		return []string{"Не удалось прочитать блокировки службы: " + err.Error()}
	}
	now := b.now()
	var names []string
	for _, r := range open {
		if r.State != store.BlockActive {
			continue
		}
		if t, err := b.parse(r.RestoreAt); err == nil && now.After(t.Add(time.Minute)) {
			names = append(names, r.FIO)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("Не снята блокировка по окончании срока: %s", nameList(names))}
}

// strayBlocks reads every active employee from PERCo and lists those blocked without an open row of this
// service: left over from a lost database, or set by hand. They are only reported, never touched.
func (b *Blocker) strayBlocks(ctx context.Context) []string {
	staff, err := b.st.AllStaff()
	if err != nil {
		return []string{"Не удалось прочитать список сотрудников: " + err.Error()}
	}
	open, _ := b.st.OpenBlocks()
	owned := map[int64]bool{}
	for _, r := range open {
		owned[r.UserID] = true
	}

	var (
		mu      sync.Mutex
		stray   []string
		failed  int
		wg      sync.WaitGroup
		jobs    = make(chan store.Staff)
		checked int
	)
	for i := 0; i < checkWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				blocked, _, err := b.api.StaffBlockState(ctx, s.ID)
				mu.Lock()
				switch {
				case err != nil:
					failed++
				case blocked && !owned[s.ID]:
					stray = append(stray, s.FIO)
				}
				checked++
				mu.Unlock()
			}
		}()
	}
	for _, s := range staff {
		if !s.Active {
			continue
		}
		select {
		case jobs <- s:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()

	var out []string
	if failed > 0 {
		out = append(out, fmt.Sprintf("Не удалось проверить блокировку у %d сотрудников", failed))
	}
	if len(stray) > 0 {
		slices.Sort(stray)
		out = append(out, fmt.Sprintf("Заблокированы вне службы (%d): %s", len(stray), nameList(stray)))
	}
	return out
}

// unknownCards finds employees whose card the controller rejected as unregistered in the last day although
// PERCo knows the card (the event carries the employee): the card never reached the controller. The problem
// stays until the person has a successful pass after the rejection, so a later check cannot report "all clear"
// merely because nobody has swiped since.
func (b *Blocker) unknownCards(now time.Time) []string {
	from := now.Add(-24 * time.Hour)
	evs, err := b.st.Events(from.Format("2006-01-02"), now.Format("2006-01-02"))
	if err != nil {
		return []string{"Не удалось прочитать события: " + err.Error()}
	}
	fromTS := from.Format("2006-01-02 15:04:05")
	lastReject := map[int64]string{} // user → time of the latest rejection
	lastPass := map[int64]string{}   // user → time of the latest successful pass
	fio := map[int64]string{}
	var order []int64
	for _, e := range evs { // ordered by time
		if e.UserID == 0 || e.TS < fromTS {
			continue
		}
		switch {
		case e.Kind == store.KindOther && e.Code == 1:
			if _, seen := lastReject[e.UserID]; !seen {
				order = append(order, e.UserID)
			}
			lastReject[e.UserID], fio[e.UserID] = e.TS, e.FIO
		case e.Kind == store.KindPass:
			lastPass[e.UserID] = e.TS
		}
	}
	var names []string
	for _, u := range order {
		if lastPass[u] > lastReject[u] {
			continue // passed since: the card works
		}
		names = append(names, fio[u])
	}
	if len(names) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("Контроллер не знает карты (%d), с тех пор не прошли: %s. Загрузите конфигурацию в контроллер в PERCo-Web", len(names), nameList(names))}
}

func nameList(names []string) string {
	if len(names) <= maxNames {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:maxNames], ", ") + fmt.Sprintf(" и ещё %d", len(names)-maxNames)
}
