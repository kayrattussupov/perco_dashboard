package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"perco-meal/internal/perco"
)

const (
	backupKeep   = 7                // daily copies of the local database kept
	backupKey    = "backup_at"      // KV: time of the last successful copy
	lowDiskBytes = 1 << 30          // below this the dashboard warns
	staleData    = 2 * time.Minute  // events older than this are reported as stale
	backupEvery  = 30 * time.Minute // how often the loop checks whether today's copy exists
)

// ServiceHealth is the "Состояние службы" panel: how fresh the data is, the load this service puts on PERCo,
// and the local resources it depends on.
type ServiceHealth struct {
	DataAgeS   float64     `json:"data_age_s"` // since the last successful event poll; -1 before the first one
	PollError  string      `json:"poll_error"`
	PERCo      perco.Stats `json:"perco"`
	DiskFreeGB float64     `json:"disk_free_gb"` // -1 when unknown
	BackupAt   string      `json:"backup_at"`
	Warnings   []string    `json:"warnings"`
}

// Health reports the service's own state for the dashboard.
func (e *Engine) Health() ServiceHealth {
	h := ServiceHealth{DataAgeS: -1, DiskFreeGB: -1, BackupAt: e.st.GetKV(backupKey), Warnings: []string{}}
	e.mu.RLock()
	if !e.lastOK.IsZero() {
		h.DataAgeS = time.Since(e.lastOK).Seconds()
	}
	h.PollError = e.lastErr
	e.mu.RUnlock()
	if e.pc != nil {
		h.PERCo = e.pc.Stats()
	}
	if free := diskFree(filepath.Dir(e.cfg.DB)); free >= 0 {
		h.DiskFreeGB = float64(free) / (1 << 30)
		if free < lowDiskBytes {
			h.Warnings = append(h.Warnings, fmt.Sprintf("Мало места на диске: %.1f ГБ — служба может перестать сохранять проходы", h.DiskFreeGB))
		}
	}
	if h.DataAgeS > staleData.Seconds() {
		h.Warnings = append(h.Warnings, fmt.Sprintf("Проходы не обновлялись %d мин", int(h.DataAgeS/60)))
	}
	if t, err := time.ParseInLocation(perco.TimeLayout, h.BackupAt, e.loc); err == nil && e.Now().Sub(t) > 48*time.Hour {
		h.Warnings = append(h.Warnings, "Резервная копия базы службы не делалась больше двух суток")
	}
	return h
}

// backupLoop makes one copy of the local database per day and keeps the last backupKeep.
func (e *Engine) backupLoop(ctx context.Context) error {
	dir := filepath.Join(filepath.Dir(e.cfg.DB), "backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := filepath.Join(dir, "perco-meal-"+e.Now().Format("2006-01-02")+".db")
	if _, err := os.Stat(name); err == nil {
		return nil
	}
	tmp := name + ".tmp"
	os.Remove(tmp)
	if err := e.st.BackupTo(tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("резервная копия: %w", err)
	}
	if err := os.Rename(tmp, name); err != nil {
		return fmt.Errorf("резервная копия: %w", err)
	}
	e.st.SetKV(backupKey, e.Now().Format(perco.TimeLayout))
	pruneBackups(dir, backupKeep)
	return nil
}

func pruneBackups(dir string, keep int) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, en := range ents {
		if n := en.Name(); strings.HasPrefix(n, "perco-meal-") && strings.HasSuffix(n, ".db") {
			names = append(names, n)
		}
	}
	slices.Sort(names) // dates in the name sort chronologically
	for len(names) > keep {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}
