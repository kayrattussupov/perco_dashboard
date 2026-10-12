// perco-meal: canteen dashboard for PERCo-Web, runs as a Windows service.
//
//	perco-meal.exe                 run in the console
//	perco-meal.exe install|uninstall|start|stop|restart
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kardianos/service"

	"perco-meal/internal/config"
	"perco-meal/internal/engine"
	"perco-meal/internal/perco"
	"perco-meal/internal/store"
	"perco-meal/internal/web"
)

type program struct {
	cfgPath string
	lg      *log.Logger
	cancel  context.CancelFunc
	done    chan struct{}
}

func (p *program) Start(service.Service) error {
	cfg, err := config.Load(p.cfgPath)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(cfg.DB) {
		cfg.DB = filepath.Join(filepath.Dir(p.cfgPath), cfg.DB)
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return fmt.Errorf("база %s: %w", cfg.DB, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})

	eng := engine.New(cfg, perco.New(cfg.Perco.URL, cfg.Perco.Login, cfg.Perco.Password), st, p.lg)
	srv := &http.Server{Addr: cfg.Listen, Handler: web.Handler(eng, p.lg), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		defer close(p.done)
		defer st.Close()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); eng.Run(ctx) }()
		go func() {
			<-ctx.Done()
			sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			srv.Shutdown(sctx)
		}()
		p.lg.Printf("запуск: PERCo %s, дашборд http://localhost%s", cfg.Perco.URL, cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.lg.Printf("веб-сервер: %v", err)
			cancel()
		}
		wg.Wait()
	}()
	return nil
}

func (p *program) Stop(service.Service) error {
	if p.cancel != nil {
		p.cancel()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
		}
	}
	p.lg.Printf("остановлено")
	return nil
}

func defaultConfigPath() string {
	if exe, err := os.Executable(); err == nil {
		c := filepath.Join(filepath.Dir(exe), "config.yaml")
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "config.yaml"
}

func main() {
	cfgPath := flag.String("config", defaultConfigPath(), "путь к config.yaml")
	flag.Parse()
	abs, err := filepath.Abs(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	var out io.Writer = newRotatingFile(filepath.Join(filepath.Dir(abs), "perco-meal.log"), 10<<20)
	if service.Interactive() {
		out = io.MultiWriter(os.Stdout, out)
	}
	lg := log.New(out, "", log.LstdFlags)

	if flag.Arg(0) == "unblock-all" {
		os.Exit(unblockAll(abs, lg))
	}

	prg := &program{cfgPath: abs, lg: lg}
	svc, err := service.New(prg, &service.Config{
		Name:        "PercoMeal",
		DisplayName: "PERCo Meal Control",
		Description: "Учёт питания по проходам PERCo-Web и дашборд столовой",
		Arguments:   []string{"-config", abs},
		Option:      service.KeyValue{"StartType": "automatic", "OnFailure": "restart", "OnFailureDelayDuration": "10s"},
	})
	if err != nil {
		log.Fatal(err)
	}
	if cmd := flag.Arg(0); cmd != "" {
		if err := service.Control(svc, cmd); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
			os.Exit(1)
		}
		fmt.Printf("%s: готово\n", cmd)
		return
	}
	if err := svc.Run(); err != nil {
		lg.Printf("ошибка: %v", err)
		os.Exit(1)
	}
}

// unblockAll is the emergency exit: switch blocking off and unblock everyone the service blocked.
// Safe to run while the service is running (shared SQLite, idempotent releases).
func unblockAll(cfgPath string, lg *log.Logger) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !filepath.IsAbs(cfg.DB) {
		cfg.DB = filepath.Join(filepath.Dir(cfgPath), cfg.DB)
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	eng := engine.New(cfg, perco.New(cfg.Perco.URL, cfg.Perco.Login, cfg.Perco.Password), st, lg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	released, failed, err := eng.Blocker().SetMode(ctx, engine.ModeOff, "команда unblock-all")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("Режим блокировки: ВЫКЛ. Разблокировано: %d, не удалось: %d\n", released, failed)
	if failed > 0 {
		fmt.Println("Оставшихся разблокируйте в PERCo: Сотрудники → фильтр «Заблокированные».")
		return 2
	}
	return 0
}

// rotatingFile is an append-only log that keeps one previous file (.1) once size exceeds max.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func newRotatingFile(path string, max int64) *rotatingFile {
	r := &rotatingFile{path: path, max: max}
	r.open()
	return r
}

func (r *rotatingFile) open() {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	r.f = f
	if fi, err := f.Stat(); err == nil {
		r.size = fi.Size()
	}
}

func (r *rotatingFile) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil && r.size+int64(len(b)) > r.max {
		r.f.Close()
		os.Remove(r.path + ".1")
		os.Rename(r.path, r.path+".1")
		r.f, r.size = nil, 0
		r.open()
	}
	if r.f == nil {
		return len(b), nil
	}
	n, err := r.f.Write(b)
	r.size += int64(n)
	return n, err
}
