package meals

import (
	"slices"
	"testing"
	"time"
)

// Windows as configured in the customer's PERCo (schedules 12, 13, 14).
var live = []Meal{
	{Key: "b", Name: "Завтрак", TemplateID: 3, Intervals: []Interval{{25500, 29700}}},
	{Key: "l", Name: "Обед", TemplateID: 4, Intervals: []Interval{{43200, 48600}}},
	{Key: "d", Name: "Ужин", TemplateID: 5, Intervals: []Interval{{66600, 70200}}},
}

func at(hh, mm, ss int) time.Time {
	return time.Date(2026, 10, 8, hh, mm, ss, 0, time.UTC)
}

func TestAt(t *testing.T) {
	cases := []struct {
		t    time.Time
		want string
	}{
		{at(7, 4, 59), ""},
		{at(7, 5, 0), "b"},
		{at(8, 14, 59), "b"},
		{at(8, 15, 0), ""},
		{at(12, 0, 0), "l"},
		{at(13, 29, 59), "l"},
		{at(16, 12, 9), ""}, // Рамадан window — not a restricted meal
		{at(18, 30, 0), "d"},
		{at(19, 30, 0), ""},
	}
	for _, c := range cases {
		if got := At(live, c.t); got != c.want {
			t.Errorf("At(%s) = %q, want %q", c.t.Format("15:04:05"), got, c.want)
		}
	}
}

func TestExpandAndAtFor(t *testing.T) {
	night := Expand("n", "Ночники", 6, []Interval{{64800, 68700}, {27000, 30600}, {69000, 72000}})
	if len(night) != 3 || night[0].Key != "n1" || night[0].Name != "Ночники 07:30" || night[2].Key != "n3" {
		t.Fatalf("Expand: %+v", night)
	}
	if one := Expand("b", "Завтрак", 3, []Interval{{25500, 29700}}); one[0].Key != "b" || one[0].Name != "Завтрак" {
		t.Fatalf("single interval must keep key and name: %+v", one)
	}
	all := append(slices.Clone(live), night...)
	cases := []struct {
		t    time.Time
		tpl  []int
		want string
	}{
		{at(7, 40, 0), []int{3, 4, 5}, "b"}, // breakfast holder
		{at(7, 40, 0), []int{6}, "n1"},      // night-shift holder in the overlap
		{at(8, 20, 0), []int{6}, "n1"},      // after breakfast ends, still night window
		{at(19, 7, 0), []int{6}, ""},        // gap between night intervals 19:05–19:10
		{at(19, 7, 0), []int{5}, "d"},       // dinner holder at the same time
		{at(7, 40, 0), nil, "b"},            // unknown person: first window by time
		{at(18, 10, 0), []int{3, 4, 5}, ""}, // dinner holder before 18:30: outside their windows
		{at(18, 10, 0), []int{1}, "n2"},     // no meal templates at all: time fallback
	}
	for _, c := range cases {
		if got := AtFor(all, c.t, c.tpl); got != c.want {
			t.Errorf("AtFor(%s, %v) = %q, want %q", c.t.Format("15:04"), c.tpl, got, c.want)
		}
	}
}

func TestSpan(t *testing.T) {
	m := Meal{Intervals: []Interval{{64800, 68700}, {27000, 30600}}}
	b, e, ok := m.Span()
	if !ok || FmtSec(b) != "07:30" || FmtSec(e) != "19:05" {
		t.Fatalf("Span = %s–%s %v", FmtSec(b), FmtSec(e), ok)
	}
}
