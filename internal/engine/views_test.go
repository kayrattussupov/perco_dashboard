package engine

import (
	"testing"

	"perco-meal/internal/store"
)

func ev(id int64, ts string, kind string, user int64, meal string) store.Event {
	return store.Event{ID: id, TS: ts, Day: ts[:10], Kind: kind, UserID: user, Meal: meal}
}

func TestAnnotate(t *testing.T) {
	in := []store.Event{
		ev(1, "2026-10-08 07:31:00", store.KindPass, 900, "b"),
		ev(2, "2026-10-08 07:40:00", store.KindPass, 900, "b"), // repeat in breakfast
		ev(3, "2026-10-08 07:41:00", store.KindDeny, 900, "b"),
		ev(4, "2026-10-08 12:10:00", store.KindPass, 900, "l"), // lunch is a fresh window
		ev(5, "2026-10-08 16:12:00", store.KindPass, 958, ""),  // outside meal windows
		ev(6, "2026-10-08 16:12:09", store.KindPass, 958, ""),
		ev(7, "2026-10-08 07:35:00", store.KindPass, 457, "b"),
	}
	want := []struct {
		seq    int
		repeat bool
	}{{1, false}, {2, true}, {0, true}, {1, false}, {0, false}, {0, false}, {1, false}}
	got := Annotate(in)
	for i, w := range want {
		if got[i].Seq != w.seq || got[i].Repeat != w.repeat {
			t.Errorf("event %d: seq=%d repeat=%v, want seq=%d repeat=%v", in[i].ID, got[i].Seq, got[i].Repeat, w.seq, w.repeat)
		}
	}
}

func TestAnnotateNewDay(t *testing.T) {
	got := Annotate([]store.Event{
		ev(1, "2026-10-07 07:31:00", store.KindPass, 900, "b"),
		ev(2, "2026-10-08 07:31:00", store.KindPass, 900, "b"),
	})
	if got[1].Repeat {
		t.Fatal("breakfast on the next day must not be a repeat")
	}
}
