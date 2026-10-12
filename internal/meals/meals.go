// Package meals maps times of day to meal windows taken from PERCo schedules.
package meals

import (
	"fmt"
	"slices"
	"time"
)

// Interval is a window in seconds from midnight, [Begin, End).
type Interval struct {
	Begin, End int
}

type Meal struct {
	Key        string
	Name       string
	TemplateID int
	Intervals  []Interval
}

func SecOfDay(t time.Time) int {
	return t.Hour()*3600 + t.Minute()*60 + t.Second()
}

func (m Meal) Contains(t time.Time) bool {
	s := SecOfDay(t)
	for _, iv := range m.Intervals {
		if s >= iv.Begin && s < iv.End {
			return true
		}
	}
	return false
}

// EndAt returns the end (seconds from midnight) of the interval containing t.
func (m Meal) EndAt(t time.Time) (int, bool) {
	s := SecOfDay(t)
	for _, iv := range m.Intervals {
		if s >= iv.Begin && s < iv.End {
			return iv.End, true
		}
	}
	return 0, false
}

// NextBegin returns the earliest interval start (seconds from midnight) at or after `after` among the
// windows the person holds. Like AtFor, it falls back to all windows when their templates are unknown.
func NextBegin(ms []Meal, after int, templates []int) (int, bool) {
	holds := false
	for _, m := range ms {
		if slices.Contains(templates, m.TemplateID) {
			holds = true
			break
		}
	}
	best, found := 0, false
	for _, m := range ms {
		if holds && !slices.Contains(templates, m.TemplateID) {
			continue
		}
		for _, iv := range m.Intervals {
			if iv.Begin >= after && (!found || iv.Begin < best) {
				best, found = iv.Begin, true
			}
		}
	}
	return best, found
}

// Find returns the meal with the given key.
func Find(ms []Meal, key string) (Meal, bool) {
	for _, m := range ms {
		if m.Key == key {
			return m, true
		}
	}
	return Meal{}, false
}

// AtFor picks the window for a person. If they hold any meal template, only their own windows count,
// so overlapping templates (Ночники vs Завтрак) are attributed correctly and a pass between their
// intervals is "outside" rather than charged to someone else's window. Only when their templates are
// unknown does it fall back to the first window by time.
func AtFor(ms []Meal, t time.Time, templates []int) string {
	holds := false
	for _, m := range ms {
		if slices.Contains(templates, m.TemplateID) {
			holds = true
			if m.Contains(t) {
				return m.Key
			}
		}
	}
	if holds {
		return ""
	}
	return At(ms, t)
}

// Expand turns a template with several intervals into one window per interval, so each interval
// allows its own pass: "Ночники" 07:30/18:00/19:10 become keys n1, n2, n3.
func Expand(key, name string, templateID int, ivs []Interval) []Meal {
	if len(ivs) == 1 {
		return []Meal{{Key: key, Name: name, TemplateID: templateID, Intervals: ivs}}
	}
	sorted := slices.Clone(ivs)
	slices.SortFunc(sorted, func(a, b Interval) int { return a.Begin - b.Begin })
	out := make([]Meal, 0, len(sorted))
	for i, iv := range sorted {
		out = append(out, Meal{Key: fmt.Sprintf("%s%d", key, i+1), Name: name + " " + FmtSec(iv.Begin),
			TemplateID: templateID, Intervals: []Interval{iv}})
	}
	return out
}

// At returns the key of the meal whose window contains t, or "" outside all windows.
func At(ms []Meal, t time.Time) string {
	for _, m := range ms {
		if m.Contains(t) {
			return m.Key
		}
	}
	return ""
}

// Span returns the earliest begin and latest end of the meal's intervals.
func (m Meal) Span() (begin, end int, ok bool) {
	if len(m.Intervals) == 0 {
		return 0, 0, false
	}
	begin, end = m.Intervals[0].Begin, m.Intervals[0].End
	for _, iv := range m.Intervals[1:] {
		begin = min(begin, iv.Begin)
		end = max(end, iv.End)
	}
	return begin, end, true
}

func FmtSec(s int) string {
	return fmt.Sprintf("%02d:%02d", s/3600, s%3600/60)
}
