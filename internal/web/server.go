// Package web serves the canteen dashboard and its JSON/SSE API.
package web

import (
	"embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"perco-meal/internal/engine"
)

//go:embed static
var static embed.FS

func Handler(e *engine.Engine, lg *log.Logger) http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServerFS(sub))

	mux.HandleFunc("GET /api/today", func(w http.ResponseWriter, r *http.Request) {
		t, err := e.Today()
		if err != nil {
			lg.Printf("today: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(t)
	})

	mux.HandleFunc("GET /api/stream", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		ch := e.Subscribe()
		defer e.Unsubscribe(ch)
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		fmt.Fprint(w, "retry: 3000\n\n")
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
				fmt.Fprint(w, "data: changed\n\n")
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
			}
			fl.Flush()
		}
	})

	mux.HandleFunc("GET /api/photo/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		p, err := e.Photo(id)
		if err != nil || p == "" {
			http.NotFound(w, r)
			return
		}
		if i := strings.Index(p, "base64,"); i >= 0 {
			p = p[i+len("base64,"):]
		}
		img, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", http.DetectContentType(img))
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Write(img)
	})

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, e.Health())
	})

	bl := e.Blocker()
	guard := newPINGuard()
	mux.HandleFunc("GET /api/blocking", func(w http.ResponseWriter, r *http.Request) {
		s, err := bl.Status()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	// admin wraps state-changing blocking endpoints: PIN required, brute force throttled, every call audited.
	admin := func(h func(w http.ResponseWriter, r *http.Request, actor string, body map[string]string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if !guard.allowed(ip) {
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Слишком много неверных PIN. Подождите минуту."})
				return
			}
			var body map[string]string
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "неверный запрос"})
				return
			}
			if !bl.CheckPIN(body["pin"]) {
				guard.fail(ip)
				msg := "Неверный PIN"
				if !bl.PINSet() {
					msg = "PIN не задан в config.yaml (blocking.admin_pin) — управление отключено"
				}
				lg.Printf("неверный PIN с %s", ip)
				writeJSON(w, http.StatusForbidden, map[string]string{"error": msg})
				return
			}
			guard.reset(ip)
			h(w, r, "дашборд "+ip, body)
		}
	}
	mux.HandleFunc("POST /api/blocking/mode", admin(func(w http.ResponseWriter, r *http.Request, actor string, body map[string]string) {
		rel, failed, err := bl.SetMode(r.Context(), body["mode"], actor)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"released": rel, "failed": failed})
	}))
	mux.HandleFunc("POST /api/blocking/release-all", admin(func(w http.ResponseWriter, r *http.Request, actor string, _ map[string]string) {
		rel, failed := bl.ReleaseAll(r.Context(), actor, "снять все")
		writeJSON(w, http.StatusOK, map[string]int{"released": rel, "failed": failed})
	}))
	mux.HandleFunc("POST /api/blocking/release/{id}", admin(func(w http.ResponseWriter, r *http.Request, actor string, _ map[string]string) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err := bl.ReleaseOne(r.Context(), id, actor); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"result": "ok"})
	}))

	mux.HandleFunc("GET /api/report.csv", func(w http.ResponseWriter, r *http.Request) {
		now := e.Now()
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		if from == "" {
			from = now.Format("2006-01") + "-01"
		}
		if to == "" {
			to = now.Format("2006-01-02")
		}
		rows, ms, err := e.Report(from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="pitanie_%s_%s.csv"`, from, to))
		w.Write([]byte("\xEF\xBB\xBF")) // BOM so Excel opens UTF-8 correctly
		cw := csv.NewWriter(w)
		cw.Comma = ';'
		head := []string{"Дата", "Сотрудник", "Подразделение"}
		for _, m := range ms {
			head = append(head, m.Name)
		}
		head = append(head, "Вне окон", "Повторные проходы", "Отказы контроллера")
		cw.Write(head)
		for _, r := range rows {
			rec := []string{r.Day, r.FIO, r.Division}
			for _, m := range ms {
				rec = append(rec, strconv.Itoa(r.Passes[m.Key]))
			}
			rec = append(rec, strconv.Itoa(r.Outside), strconv.Itoa(r.Repeats), strconv.Itoa(r.Denied))
			cw.Write(rec)
		}
		cw.Flush()
	})
	return mux
}
