package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"perco-meal/internal/store"
)

// selfEvent is what PERCo logs for an is_block change made under the service's login.
func (r *rig) selfEvent(code int, user int64) store.Event {
	r.next++
	ev := store.Event{ID: r.next, TS: r.now.Format("2006-01-02 15:04:05"), Day: r.now.Format("2006-01-02"),
		Kind: store.KindStaffOp, Code: code, UserID: user, FIO: "Сотрудник", Actor: "meal-service"}
	r.st.InsertEvent(ev)
	return ev
}

func foreignRig(t *testing.T) *rig {
	r := newRig(t, ModeOn)
	r.b.startWriteJournal()
	r.now = r.now.Add(time.Second)
	return r
}

func TestOwnWritesAreNotForeign(t *testing.T) {
	r := foreignRig(t)
	r.pass(900)
	r.now = r.now.Add(10 * time.Second)
	r.b.NoteSelfAction(r.selfEvent(evStaffBlocked, 900))
	if r.st.GetKV(foreignKey) != "" {
		t.Fatalf("our own block flagged: %s", r.st.GetKV(foreignKey))
	}
}

// A block under our login that we never sent stops new blocks; releases still run; a mode switch clears it.
func TestForeignWriteStopsNewBlocks(t *testing.T) {
	r := foreignRig(t)
	r.pass(900) // ours, owed an unblock later
	r.now = r.now.Add(time.Minute)
	r.b.NoteSelfAction(r.selfEvent(evStaffBlocked, 777))
	s, _ := r.b.Status()
	if !strings.Contains(strings.Join(s.Warnings, " "), "вторая копия службы") {
		t.Fatalf("dashboard must warn: %v", s.Warnings)
	}
	r.now = r.now.Add(time.Minute)
	r.pass(901)
	if r.api.blocked[901] {
		t.Fatal("no new blocks while another writer is suspected")
	}
	r.b.ReleaseAll(context.Background(), "test", "проверка")
	if r.api.blocked[900] {
		t.Fatal("releases must still run: unblocking is the safe direction")
	}
	r.b.SetMode(context.Background(), ModeOn, "дашборд")
	if r.st.GetKV(foreignKey) != "" {
		t.Fatal("a mode switch by a person clears the warning")
	}
}

func TestForeignCheckSkipsTestUsersAndOldEvents(t *testing.T) {
	r := foreignRig(t)
	r.b.NoteSelfAction(r.selfEvent(evStaffUnblocked, 960)) // perco-bench / test mode
	old := r.selfEvent(evStaffBlocked, 777)
	old.TS = r.now.Add(-time.Hour).Format("2006-01-02 15:04:05") // before the write journal existed
	r.b.NoteSelfAction(old)
	other := r.selfEvent(evStaffBlocked, 778)
	other.Actor = "admin" // a person under their own login: operator rules apply, not this check
	r.b.NoteSelfAction(other)
	if r.st.GetKV(foreignKey) != "" {
		t.Fatalf("flagged: %s", r.st.GetKV(foreignKey))
	}
}
