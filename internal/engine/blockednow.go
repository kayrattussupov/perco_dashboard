package engine

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"perco-meal/internal/perco"
	"perco-meal/internal/store"
)

const blockedEvery = 10 * time.Second // how often "who is blocked in PERCo right now" is read

// blockedSnapshot is the last reading of the blocked employees, guarded by Blocker.hmu.
type blockedSnapshot struct {
	people []perco.BlockedPerson
	at     time.Time
	err    string
	key    string // the set of blocked ids; a change is pushed to the dashboard
}

// BlockedNow is everyone blocked in PERCo at this moment, whoever blocked them.
type BlockedNow struct {
	Known     bool     `json:"known"`   // false until the first successful reading
	Count     int      `json:"count"`   // all blocked now
	Service   int      `json:"service"` // blocked by this service (it owes the unblock)
	Manual    int      `json:"manual"`  // blocked by an operator or left over
	Names     []string `json:"names"`   // the manual ones, to see who to look at
	CheckedAt string   `json:"checked_at"`
	Error     string   `json:"error"`
}

// BlockedLoop reads the blocked employees from PERCo in the background; it never fails the loop: an error is
// part of what the dashboard shows.
func (b *Blocker) BlockedLoop(ctx context.Context) error {
	people, err := b.api.BlockedStaff(ctx)
	if ctx.Err() != nil {
		return nil
	}
	now := b.now()
	ids := make([]string, 0, len(people))
	for _, p := range people {
		ids = append(ids, strconv.FormatInt(p.ID, 10))
	}
	slices.Sort(ids)
	key := strings.Join(ids, ",")

	b.hmu.Lock()
	changed := (err == nil && key != b.blockedNow.key) || (err != nil) != (b.blockedNow.err != "")
	if err != nil {
		b.blockedNow.err = err.Error()
	} else {
		b.blockedNow = blockedSnapshot{people: people, at: now, key: key}
	}
	b.hmu.Unlock()
	if changed {
		b.notify()
	}
	return nil
}

// BlockedNowStatus combines the last reading with the service's own open rows.
func (b *Blocker) BlockedNowStatus(open []store.Block) BlockedNow {
	b.hmu.Lock()
	snap := b.blockedNow
	b.hmu.Unlock()
	out := BlockedNow{Names: []string{}, Error: snap.err}
	if snap.at.IsZero() {
		return out
	}
	out.Known = true
	out.CheckedAt = snap.at.Format(perco.TimeLayout)
	owned := map[int64]bool{}
	for _, r := range open {
		if r.State == store.BlockActive || r.State == store.BlockPending {
			owned[r.UserID] = true
		}
	}
	for _, p := range snap.people {
		out.Count++
		if owned[p.ID] {
			out.Service++
			continue
		}
		out.Manual++
		out.Names = append(out.Names, b.personName(p))
	}
	slices.Sort(out.Names)
	if len(out.Names) > maxNames {
		out.Names = append(out.Names[:maxNames], "…")
	}
	return out
}

// personName is the employee's name: from PERCo's table, else from the local staff cache.
func (b *Blocker) personName(p perco.BlockedPerson) string {
	if p.FIO != "" {
		return p.FIO
	}
	if all, err := b.st.AllStaff(); err == nil {
		for _, s := range all {
			if s.ID == p.ID {
				return s.FIO
			}
		}
	}
	return "№" + strconv.FormatInt(p.ID, 10)
}
