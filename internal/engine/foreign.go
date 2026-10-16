package engine

import (
	"context"
	"fmt"
	"slices"
	"time"

	"perco-meal/internal/perco"
	"perco-meal/internal/store"
)

// Second writer detection. Every is_block write of this service is recorded locally before it is sent; PERCo then
// logs it as an operator action under this service's login. An action under our login that we did not record
// means someone else writes as us — a second copy of the service, or a person using its login. Then this service
// stops setting new blocks (releases continue: that direction is safe) until a person switches the mode on the
// dashboard.
const (
	foreignKey     = "foreign_writer" // KV: description of the first unexplained action; empty when none
	writesSinceKey = "own_writes_since"
	ownWriteSlack  = 5 * time.Minute // PERCo's event time vs. our record (same PC, plus clock tolerance)
)

// startWriteJournal marks when the write journal began (first start of this version); idempotent.
func (b *Blocker) startWriteJournal() {
	if b.st.GetKV(writesSinceKey) == "" {
		b.st.SetKV(writesSinceKey, b.nowTS())
	}
}

// setBlocked records the write, then sends it.
func (b *Blocker) setBlocked(ctx context.Context, userID int64, blocked bool) error {
	if err := b.st.RecordWrite(b.nowTS(), userID, blocked); err != nil {
		return fmt.Errorf("журнал записей: %w", err)
	}
	return b.api.SetBlocked(ctx, userID, blocked)
}

// NoteSelfAction checks a block/unblock event logged under this service's login against the recorded writes.
func (b *Blocker) NoteSelfAction(ev store.Event) {
	if ev.Actor != b.self || slices.Contains(b.cfg.Blocking.TestUserIDs, ev.UserID) {
		return // perco-bench and test mode work with the test employees
	}
	since := b.st.GetKV(writesSinceKey)
	if since == "" || ev.TS < since { // older events predate the write journal
		return
	}
	at, err := b.parse(ev.TS)
	if err != nil {
		return
	}
	blocked := ev.Code == evStaffBlocked
	ok, err := b.st.HasWrite(ev.UserID, blocked, at.Add(-ownWriteSlack).Format(perco.TimeLayout), at.Add(ownWriteSlack).Format(perco.TimeLayout))
	if err != nil || ok {
		return
	}
	if b.st.GetKV(foreignKey) != "" {
		return
	}
	what := "разблокировал"
	if blocked {
		what = "заблокировал"
	}
	msg := fmt.Sprintf("%s под логином службы %q %s %s, но эта служба этого не делала", ev.TS[11:16], b.self, what, ev.FIO)
	b.st.SetKV(foreignKey, msg)
	b.log.Printf("ВТОРОЙ ЭКЗЕМПЛЯР: %s. Новые блокировки остановлены.", msg)
	b.notify()
}

func (b *Blocker) foreignWarning() string {
	msg := b.st.GetKV(foreignKey)
	if msg == "" {
		return ""
	}
	return "Похоже, в PERCo пишет вторая копия службы: " + msg + ". Новые блокировки остановлены. " +
		"Найдите и остановите вторую службу PercoMeal (или человека под этим логином), затем переключите режим блокировки на дашборде."
}
