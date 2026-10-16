package store

import (
	"path/filepath"
	"testing"
)

// A row stored before card numbers were kept gets the number on the next re-read, without counting as new.
func TestInsertEventFillsCardOnReread(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := Event{ID: 1, TS: "2026-10-16 12:37:57", Day: "2026-10-16", Kind: KindDeny, Code: 1, Name: "Идентификатор не зарегистрирован"}
	if isNew, err := st.InsertEvent(ev); err != nil || !isNew {
		t.Fatalf("first insert: new=%v err=%v", isNew, err)
	}
	ev.Card = "365074375059"
	if isNew, err := st.InsertEvent(ev); err != nil || isNew {
		t.Fatalf("re-read must not be new: new=%v err=%v", isNew, err)
	}
	ev.Card = "999"
	st.InsertEvent(ev) // a stored number is never overwritten
	got, err := st.Events("2026-10-16", "2026-10-16")
	if err != nil || len(got) != 1 || got[0].Card != "365074375059" {
		t.Fatalf("card = %+v, err %v", got, err)
	}
}
