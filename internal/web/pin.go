package web

import (
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	pinMaxFails = 5
	pinLockout  = time.Minute
)

// pinGuard throttles PIN guessing per client IP.
type pinGuard struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func newPINGuard() *pinGuard { return &pinGuard{fails: map[string][]time.Time{}} }

func (g *pinGuard) recent(ip string) []time.Time {
	cut := time.Now().Add(-pinLockout)
	var keep []time.Time
	for _, t := range g.fails[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	g.fails[ip] = keep
	return keep
}

func (g *pinGuard) allowed(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.recent(ip)) < pinMaxFails
}

func (g *pinGuard) fail(ip string) {
	g.mu.Lock()
	g.fails[ip] = append(g.recent(ip), time.Now())
	g.mu.Unlock()
}

func (g *pinGuard) reset(ip string) {
	g.mu.Lock()
	delete(g.fails, ip)
	g.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
