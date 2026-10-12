package engine

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"perco-meal/internal/meals"
	"perco-meal/internal/perco"
	"perco-meal/internal/store"
)

type MealOut struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Begin string `json:"begin"`
	End   string `json:"end"`
}

type EventOut struct {
	store.Event
	Seq    int  `json:"seq"`    // n-th pass of this person in this meal window
	Repeat bool `json:"repeat"` // second+ pass in a window, or a controller denial
}

type StaffOut struct {
	store.Staff
	Meals []string `json:"meals"`
}

type Today struct {
	Now     string     `json:"now"`
	Day     string     `json:"day"`
	Current string     `json:"current"` // key of the meal window open now, "" otherwise
	Meals   []MealOut  `json:"meals"`
	Events  []EventOut `json:"events"`
	Staff   []StaffOut `json:"staff"`
	Alerts  []string   `json:"alerts"`
}

// Annotate numbers passes per (person, day, meal) and flags repeats. Events must be time-ordered.
func Annotate(evs []store.Event) []EventOut {
	seen := map[string]int{}
	out := make([]EventOut, 0, len(evs))
	for _, e := range evs {
		o := EventOut{Event: e}
		switch e.Kind {
		case store.KindPass:
			if e.Meal != "" {
				k := fmt.Sprintf("%d|%s|%s", e.UserID, e.Day, e.Meal)
				seen[k]++
				o.Seq = seen[k]
				o.Repeat = o.Seq > 1
			}
		case store.KindDeny:
			o.Repeat = true
		}
		out = append(out, o)
	}
	return out
}

func (e *Engine) Today() (*Today, error) {
	now := e.Now()
	day := now.Format("2006-01-02")
	ms := e.Meals()
	t := &Today{Now: now.Format(perco.TimeLayout), Day: day, Current: meals.At(ms, now), Alerts: []string{},
		Meals: []MealOut{}, Staff: []StaffOut{}} // never null in JSON: a fresh install with PERCo down must still render
	tplMeal := map[int][]string{} // one template can give several windows (Ночники: n1, n2, n3)
	for _, m := range ms {
		tplMeal[m.TemplateID] = append(tplMeal[m.TemplateID], m.Key)
		mo := MealOut{Key: m.Key, Name: m.Name}
		if b, en, ok := m.Span(); ok {
			mo.Begin, mo.End = meals.FmtSec(b), meals.FmtSec(en)
		}
		t.Meals = append(t.Meals, mo)
	}

	evs, err := e.st.Events(day, day)
	if err != nil {
		return nil, err
	}
	shown := evs[:0:0]
	for _, ev := range evs {
		if ev.Kind == store.KindPass || ev.Kind == store.KindDeny || ev.Kind == store.KindDoor {
			shown = append(shown, ev)
		}
	}
	t.Events = Annotate(shown)

	inEvents := map[int64]bool{}
	for _, ev := range shown {
		inEvents[ev.UserID] = true
	}
	all, err := e.st.AllStaff()
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		so := StaffOut{Staff: s, Meals: []string{}}
		for _, id := range s.Templates {
			for _, k := range tplMeal[id] {
				if !slices.Contains(so.Meals, k) {
					so.Meals = append(so.Meals, k)
				}
			}
		}
		if (s.Active && len(so.Meals) > 0) || inEvents[s.ID] {
			t.Staff = append(t.Staff, so)
		}
	}

	e.mu.RLock()
	lastOK, lastErr := e.lastOK, e.lastErr
	e.mu.RUnlock()
	if lastErr != "" && time.Since(lastOK) > 15*time.Second {
		t.Alerts = append(t.Alerts, "Нет связи с PERCo-Web — данные могут быть неполными. "+lastErr)
	}
	if e.cfg.ResetMonitor.Enabled && t.Current != "" {
		for _, m := range ms {
			if m.Key != t.Current {
				continue
			}
			b, _, _ := m.Span()
			start := dayStart(now).Add(time.Duration(b) * time.Second)
			if !e.resetSeen(start, start) {
				t.Alerts = append(t.Alerts, fmt.Sprintf("Перед «%s» не зафиксирован сброс зональности — сотрудники, прошедшие ранее, могут получить отказ. Проверьте задачу сброса в PERCo.", m.Name))
			}
		}
	}
	return t, nil
}

type ReportRow struct {
	Day      string
	FIO      string
	Division string
	Passes   map[string]int // meal key → passes
	Outside  int            // passes outside any meal window
	Repeats  int
	Denied   int
}

// Report aggregates passes per person per day for [fromDay, toDay].
func (e *Engine) Report(fromDay, toDay string) ([]ReportRow, []MealOut, error) {
	evs, err := e.st.Events(fromDay, toDay)
	if err != nil {
		return nil, nil, err
	}
	all, err := e.st.AllStaff()
	if err != nil {
		return nil, nil, err
	}
	div := map[int64]string{}
	for _, s := range all {
		div[s.ID] = s.Division
	}
	rows := map[string]*ReportRow{}
	for _, o := range Annotate(evs) {
		if o.Kind != store.KindPass && o.Kind != store.KindDeny {
			continue
		}
		k := fmt.Sprintf("%s|%d", o.Day, o.UserID)
		r := rows[k]
		if r == nil {
			r = &ReportRow{Day: o.Day, FIO: o.FIO, Division: div[o.UserID], Passes: map[string]int{}}
			rows[k] = r
		}
		switch {
		case o.Kind == store.KindDeny:
			r.Denied++
		case o.Meal == "":
			r.Outside++
		default:
			r.Passes[o.Meal]++
			if o.Repeat {
				r.Repeats++
			}
		}
	}
	out := make([]ReportRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		return out[i].FIO < out[j].FIO
	})
	var mo []MealOut
	for _, m := range e.Meals() {
		mo = append(mo, MealOut{Key: m.Key, Name: m.Name})
	}
	return out, mo, nil
}
