package engine

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"perco-meal/internal/config"
	"perco-meal/internal/store"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DB: filepath.Join(dir, "perco-meal.db"), UTCOffsetHours: 5, DeviceIDs: []int{2}}
	st, err := store.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(cfg, nil, st, log.New(io.Discard, "", 0))
}

// One copy per day, a second run the same day does nothing, only the last backupKeep are kept.
func TestBackupDailyAndPruned(t *testing.T) {
	e := newTestEngine(t)
	dir := filepath.Join(filepath.Dir(e.cfg.DB), "backup")
	for i := 0; i < backupKeep+2; i++ { // older copies left by previous days
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "perco-meal-2026-01-0"+string(rune('1'+i))+".db"), []byte("x"), 0o644)
	}
	if err := e.backupLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.backupLoop(context.Background()); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != backupKeep {
		t.Fatalf("%d copies kept, want %d", len(ents), backupKeep)
	}
	today := "perco-meal-" + e.Now().Format("2006-01-02") + ".db"
	if fi, err := os.Stat(filepath.Join(dir, today)); err != nil || fi.Size() == 0 {
		t.Fatalf("today's copy: %v", err)
	}
	if e.Health().BackupAt == "" {
		t.Fatal("the panel must show the backup time")
	}
}

func TestHealthWarnsStaleData(t *testing.T) {
	e := newTestEngine(t)
	if h := e.Health(); h.DataAgeS != -1 || len(h.Warnings) != 0 {
		t.Fatalf("fresh start: %+v", h)
	}
	e.mu.Lock()
	e.lastOK = time.Now().Add(-5 * time.Minute)
	e.mu.Unlock()
	if h := e.Health(); !strings.Contains(strings.Join(h.Warnings, " "), "не обновлялись 5 мин") {
		t.Fatalf("stale data must be a warning: %+v", h.Warnings)
	}
}
