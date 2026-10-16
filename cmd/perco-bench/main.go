// perco-bench measures how long PERCo takes to apply a batch block and how PERCo-Web behaves under read load.
// It touches only employees listed in blocking.test_user_ids and always unblocks them before it exits.
//
//	perco-bench.exe -ids 960,961,962,963,964             check only: nothing is written
//	perco-bench.exe -ids 960,961,962,963,964 -yes        block/unblock the batch, measure each stage
//	perco-bench.exe -load -yes                           step read load 1 → 2 → 4 requests/s, stops on slowdown
//	perco-bench.exe -find Тест                           list employees whose name contains the text (read only)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"perco-meal/internal/config"
	"perco-meal/internal/perco"
)

const (
	maxIDs      = 10
	maxRPS      = 5               // hard cap of the read load test
	pollEvery   = time.Second     // read-back cadence while waiting for a stage
	cleanupFor  = 5 * time.Minute // how long the final unblock keeps retrying
	slowP95     = 3 * time.Second // the load test stops once PERCo answers this slowly
	stepPause   = 30 * time.Second
	reqTimeout  = 15 * time.Second
	autoTryWait = 20 * time.Second // auto format: how long to wait for read-back before trying the next encoding
)

type api interface {
	StaffBlockState(ctx context.Context, id int64) (blocked, active bool, err error)
	SetBlocked(ctx context.Context, id int64, blocked bool) error
	SetBlockedMany(ctx context.Context, ids []int64, blocked bool, format string) error
	DeviceHealth(ctx context.Context, deviceIDs []int) (perco.DeviceHealth, error)
	Events(ctx context.Context, from, to time.Time, page, rows int) ([]perco.Event, error)
	BlockedStaff(ctx context.Context) ([]perco.BlockedPerson, error)
}

type bench struct {
	api     api
	devices []int
	out     *log.Logger
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	poll    time.Duration
}

func main() {
	cfgPath := flag.String("config", defaultConfigPath(), "путь к config.yaml службы")
	idsFlag := flag.String("ids", "", "id тестовых сотрудников через запятую (только из blocking.test_user_ids)")
	rounds := flag.Int("rounds", 3, "сколько раз повторить блокировку и снятие")
	hold := flag.Duration("hold", 20*time.Second, "сколько держать блокировку перед снятием")
	timeout := flag.Duration("timeout", 2*time.Minute, "предел ожидания одного этапа")
	format := flag.String("idsfmt", "auto", "как передавать список id: auto, comma, json, repeat")
	load := flag.Bool("load", false, "проверка нагрузки чтением вместо блокировок")
	steps := flag.String("steps", "1,2,4", "ступени нагрузки, запросов в секунду (не больше 5)")
	stepDur := flag.Duration("step", 30*time.Second, "длительность одной ступени")
	find := flag.String("find", "", "показать id сотрудников, в ФИО которых есть этот текст (только чтение)")
	yes := flag.Bool("yes", false, "выполнить; без флага только проверки, ничего не пишется")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	report := filepath.Join(filepath.Dir(*cfgPath), "bench-"+time.Now().Format("20060102-150405")+".txt")
	f, err := os.Create(report)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	out := log.New(io.MultiWriter(os.Stdout, f), "", log.Ltime)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	b := &bench{api: perco.New(cfg.Perco.URL, cfg.Perco.Login, cfg.Perco.Password), devices: cfg.DeviceIDs,
		out: out, now: time.Now, sleep: sleepCtx, poll: pollEvery}

	out.Printf("perco-bench: PERCo %s, оператор %s, отчёт %s", cfg.Perco.URL, cfg.Perco.Login, report)
	code := 0
	if *find != "" {
		code = findStaff(ctx, perco.New(cfg.Perco.URL, cfg.Perco.Login, cfg.Perco.Password), *find, out)
	} else if *load {
		code = b.runLoad(ctx, *steps, *stepDur, *yes)
	} else {
		ids, err := parseIDs(*idsFlag, cfg.Blocking.TestUserIDs)
		if err != nil {
			out.Printf("ОШИБКА: %v", err)
			os.Exit(2)
		}
		code = b.runBlock(ctx, ids, *rounds, *hold, *timeout, *format, *yes)
	}
	os.Exit(code)
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

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func findStaff(ctx context.Context, pc *perco.Client, text string, out *log.Logger) int {
	list, err := pc.StaffList(ctx)
	if err != nil {
		out.Printf("ОШИБКА: %v", err)
		return 1
	}
	n := 0
	for _, s := range list {
		if strings.Contains(strings.ToLower(s.FIO()), strings.ToLower(text)) {
			out.Printf("id %d  %s  активен=%v", s.ID, s.FIO(), s.IsActive == 1)
			n++
		}
	}
	out.Printf("найдено: %d", n)
	return 0
}

// parseIDs accepts only test employees: the tool must never touch real people.
func parseIDs(s string, allowed []int64) ([]int64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("не указан -ids")
	}
	var ids []int64
	for _, p := range strings.Split(s, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("-ids: %q не число", p)
		}
		if !slices.Contains(allowed, id) {
			return nil, fmt.Errorf("сотрудника %d нет в blocking.test_user_ids config.yaml — трогать можно только тестовых", id)
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > maxIDs {
		return nil, fmt.Errorf("не больше %d id за раз", maxIDs)
	}
	return ids, nil
}

// ---------- batch block ----------

type stage struct {
	API, Stored, Delivered time.Duration // from the moment the request was sent
	MaxPending             int
	Err                    string
}

func (b *bench) runBlock(ctx context.Context, ids []int64, rounds int, hold, timeout time.Duration, format string, yes bool) int {
	b.out.Printf("сотрудники: %v, повторов: %d, держать: %s", ids, rounds, hold)
	if err := b.preflight(ctx, ids); err != nil {
		b.out.Printf("ОСТАНОВЛЕНО до записи: %v", err)
		return 1
	}
	b.baseline(ctx)
	if !yes {
		b.out.Printf("проверки пройдены. Это пробный запуск: ничего не записано. Для замера добавьте -yes")
		return 0
	}

	// From here on PERCo is written to: whatever happens, unblock before exit.
	defer func() {
		if !b.cleanup(ids) {
			b.out.Printf("!!! НЕ УДАЛОСЬ СНЯТЬ БЛОКИРОВКУ с %v — снимите в PERCo вручную", ids)
		}
	}()

	if format == "auto" {
		f, err := b.detectFormat(ctx, ids)
		if err != nil {
			b.out.Printf("ОСТАНОВЛЕНО: %v", err)
			return 1
		}
		format = f
	}

	var blocks, unblocks []stage
	for r := 1; r <= rounds && ctx.Err() == nil; r++ {
		if err := b.waitSettled(ctx, timeout); err != nil {
			b.out.Printf("ОСТАНОВЛЕНО перед повтором %d: %v", r, err)
			break
		}
		s := b.measure(ctx, ids, true, format, timeout)
		b.printStage(r, "блокировка", s)
		blocks = append(blocks, s)
		if s.Err != "" {
			break
		}
		if b.sleep(ctx, hold) != nil {
			break
		}
		s = b.measure(ctx, ids, false, format, timeout)
		b.printStage(r, "снятие", s)
		unblocks = append(unblocks, s)
		if s.Err != "" {
			break
		}
	}
	b.summary("блокировка", len(ids), blocks)
	b.summary("снятие", len(ids), unblocks)
	return 0
}

// preflight refuses to write unless the test employees are active and unblocked and the controller is settled.
func (b *bench) preflight(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		blocked, active, err := b.api.StaffBlockState(ctx, id)
		if err != nil {
			return fmt.Errorf("сотрудник %d: %w", id, err)
		}
		if !active {
			return fmt.Errorf("сотрудник %d не активен", id)
		}
		if blocked {
			return fmt.Errorf("сотрудник %d уже заблокирован (возможно, оператором) — не трогаю", id)
		}
	}
	h, err := b.api.DeviceHealth(ctx, b.devices)
	if err != nil {
		return fmt.Errorf("состояние контроллера: %w", err)
	}
	if !h.Settled() {
		return fmt.Errorf("контроллер не готов: на связи=%v, в очереди=%d, не передано=%v %s", h.Connected, h.Pending, h.Errors, h.Message)
	}
	b.out.Printf("проверки: сотрудники активны и не заблокированы, контроллер на связи, очередь пуста")
	return nil
}

// baseline compares a light request (controller state) with the service's event poll (all events of the last
// 5 minutes): if the poll is much slower, PERCo's event query itself is heavy, whatever the request rate.
func (b *bench) baseline(ctx context.Context) {
	var light, heavy []time.Duration
	for range 5 {
		t := b.now()
		if _, err := b.api.DeviceHealth(ctx, b.devices); err != nil {
			b.out.Printf("фон: ошибка чтения состояния: %v", err)
			return
		}
		light = append(light, b.now().Sub(t))
		t = b.now()
		if _, err := b.api.Events(ctx, t.Add(-5*time.Minute), t.Add(time.Hour), 1, 100); err != nil {
			b.out.Printf("фон: ошибка чтения событий: %v", err)
			return
		}
		heavy = append(heavy, b.now().Sub(t))
		if b.sleep(ctx, b.poll) != nil {
			return
		}
	}
	b.out.Printf("фон: состояние контроллера (лёгкий запрос) %s", stats(light))
	b.out.Printf("фон: события за 5 минут (опрос службы) %s", stats(heavy))
	if l, h := max(percentile(light, 50), time.Millisecond), percentile(heavy, 50); h > 5*l {
		b.out.Printf("фон: опрос событий в %.0f раз медленнее лёгкого запроса — запрос событий тяжёлый для PERCo", float64(h)/float64(l))
	}
}

// detectFormat finds which "ids" encoding PERCo applies to every employee of the batch.
// A wrong encoding could hit someone else, so everyone blocked in PERCo is compared before and after each try.
func (b *bench) detectFormat(ctx context.Context, ids []int64) (string, error) {
	before, err := b.blockedSet(ctx)
	if err != nil {
		return "", fmt.Errorf("список заблокированных: %w", err)
	}
	for _, f := range []string{perco.IDsComma, perco.IDsJSON, perco.IDsRepeat} {
		b.out.Printf("пробую формат списка id: %s", f)
		err := b.api.SetBlockedMany(ctx, ids, true, f)
		ok := err == nil && b.waitStored(ctx, ids, true, autoTryWait) == nil
		if !b.cleanup(ids) {
			return "", errors.New("не удалось снять пробную блокировку")
		}
		after, err := b.blockedSet(ctx)
		if err != nil {
			return "", fmt.Errorf("список заблокированных: %w", err)
		}
		var extra []int64
		for id := range after {
			if !before[id] {
				extra = append(extra, id)
			}
		}
		if len(extra) > 0 {
			slices.Sort(extra)
			return "", fmt.Errorf("после пробы формата %s заблокированы посторонние id %v — проверьте их в PERCo и снимите вручную, если это не оператор", f, extra)
		}
		if ok {
			b.out.Printf("формат %s: PERCo применил ко всем %d", f, len(ids))
			return f, nil
		}
		b.out.Printf("формат %s не подошёл: %v", f, errOr(err, "блокировка применилась не ко всем"))
	}
	return "", errors.New("ни один формат списка id не применился ко всем — пачками PERCo не принимает")
}

func (b *bench) blockedSet(ctx context.Context) (map[int64]bool, error) {
	people, err := b.api.BlockedStaff(ctx)
	set := map[int64]bool{}
	for _, p := range people {
		set[p.ID] = true
	}
	return set, err
}

// measure sends one batch write and times three stages: the API answer, the flag stored in PERCo,
// and PERCo's queue to the controller emptied (the change delivered).
func (b *bench) measure(ctx context.Context, ids []int64, blocked bool, format string, timeout time.Duration) stage {
	var s stage
	start := b.now()
	if err := b.api.SetBlockedMany(ctx, ids, blocked, format); err != nil {
		s.Err = err.Error()
		return s
	}
	s.API = b.now().Sub(start)
	if err := b.waitStored(ctx, ids, blocked, timeout); err != nil {
		s.Err = err.Error()
		return s
	}
	s.Stored = b.now().Sub(start)
	deadline := start.Add(timeout)
	for {
		h, err := b.api.DeviceHealth(ctx, b.devices)
		if err == nil {
			s.MaxPending = max(s.MaxPending, h.Pending)
			if !h.Connected {
				s.Err = "контроллер потерял связь: " + h.Message
				return s
			}
			if h.Settled() {
				s.Delivered = b.now().Sub(start)
				return s
			}
		}
		if b.now().After(deadline) {
			s.Err = fmt.Sprintf("очередь контроллера не опустела за %s (в очереди %d)", timeout, h.Pending)
			return s
		}
		if b.sleep(ctx, b.poll) != nil {
			s.Err = "прервано"
			return s
		}
	}
}

func (b *bench) waitStored(ctx context.Context, ids []int64, want bool, timeout time.Duration) error {
	deadline := b.now().Add(timeout)
	for {
		left := 0
		for _, id := range ids {
			if got, _, err := b.api.StaffBlockState(ctx, id); err != nil || got != want {
				left++
			}
		}
		if left == 0 {
			return nil
		}
		if b.now().After(deadline) {
			return fmt.Errorf("через %s у %d из %d is_block≠%v", timeout, left, len(ids), want)
		}
		if b.sleep(ctx, b.poll) != nil {
			return errors.New("прервано")
		}
	}
}

func (b *bench) waitSettled(ctx context.Context, timeout time.Duration) error {
	deadline := b.now().Add(timeout)
	for {
		h, err := b.api.DeviceHealth(ctx, b.devices)
		if err == nil && h.Settled() {
			return nil
		}
		if b.now().After(deadline) {
			return fmt.Errorf("контроллер не готов за %s: %v %+v", timeout, err, h)
		}
		if b.sleep(ctx, b.poll) != nil {
			return errors.New("прервано")
		}
	}
}

// cleanup unblocks the test employees with a fresh context (it must run after Ctrl+C too): the batch first,
// then one by one, until PERCo shows all of them unblocked.
func (b *bench) cleanup(ids []int64) bool {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupFor)
	defer cancel()
	for {
		var left []int64
		for _, id := range ids {
			if blocked, _, err := b.api.StaffBlockState(ctx, id); err != nil || blocked {
				left = append(left, id)
			}
		}
		if len(left) == 0 {
			return true
		}
		for _, id := range left {
			if err := b.api.SetBlocked(ctx, id, false); err != nil {
				b.out.Printf("снятие %d: %v", id, err)
			}
		}
		if b.sleep(ctx, 2*time.Second) != nil {
			return false
		}
	}
}

func (b *bench) printStage(round int, what string, s stage) {
	if s.Err != "" {
		b.out.Printf("повтор %d, %s: ОШИБКА %s", round, what, s.Err)
		return
	}
	b.out.Printf("повтор %d, %s: ответ API %s, записано в PERCo %s, доставлено в контроллер %s (макс. очередь %d)",
		round, what, sec(s.API), sec(s.Stored), sec(s.Delivered), s.MaxPending)
}

func (b *bench) summary(what string, n int, ss []stage) {
	var api, stored, deliv []time.Duration
	for _, s := range ss {
		if s.Err == "" {
			api, stored, deliv = append(api, s.API), append(stored, s.Stored), append(deliv, s.Delivered)
		}
	}
	if len(deliv) == 0 {
		b.out.Printf("ИТОГ %s: нет успешных замеров", what)
		return
	}
	b.out.Printf("ИТОГ %s пачкой из %d (%d замеров): ответ API %s; записано %s; доставлено в контроллер %s",
		what, n, len(deliv), stats(api), stats(stored), stats(deliv))
}

// ---------- read load ----------

func (b *bench) runLoad(ctx context.Context, stepsFlag string, stepDur time.Duration, yes bool) int {
	var rates []int
	for _, p := range strings.Split(stepsFlag, ",") {
		r, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || r < 1 || r > maxRPS {
			b.out.Printf("ОШИБКА: ступень %q — нужно число от 1 до %d", p, maxRPS)
			return 2
		}
		rates = append(rates, r)
	}
	b.baseline(ctx)
	if !yes {
		b.out.Printf("ступени %v запросов/с по %s. Это пробный запуск: нагрузка не подавалась. Для проверки добавьте -yes", rates, stepDur)
		return 0
	}
	for i, rate := range rates {
		if i > 0 && b.sleep(ctx, stepPause) != nil {
			return 1
		}
		lat, errs := b.loadStep(ctx, rate, stepDur)
		p95 := percentile(lat, 95)
		b.out.Printf("ступень %d запр/с: выполнено %d, ошибок %d, %s", rate, len(lat), errs, stats(lat))
		if errs > 0 || p95 > slowP95 {
			b.out.Printf("ОСТАНОВЛЕНО: PERCo начал ошибаться или отвечать дольше %s на %d запр/с", slowP95, rate)
			return 0
		}
	}
	b.out.Printf("ИТОГ: все ступени пройдены без ошибок и замедления")
	return 0
}

// loadStep sends the service's real poll request (events of the last 5 minutes) at the given rate, one at a time:
// when PERCo is slower than the rate, the step simply sends fewer requests instead of piling them up.
func (b *bench) loadStep(ctx context.Context, rate int, dur time.Duration) ([]time.Duration, int) {
	var lat []time.Duration
	errs := 0
	gap := time.Second / time.Duration(rate)
	end := b.now().Add(dur)
	for b.now().Before(end) && ctx.Err() == nil {
		t := b.now()
		rctx, cancel := context.WithTimeout(ctx, reqTimeout)
		_, err := b.api.Events(rctx, t.Add(-5*time.Minute), t.Add(time.Hour), 1, 100)
		cancel()
		d := b.now().Sub(t)
		if err != nil {
			errs++
			b.out.Printf("ошибка: %v", err)
			if errs >= 3 {
				break
			}
		} else {
			lat = append(lat, d)
		}
		if d < gap && b.sleep(ctx, gap-d) != nil {
			break
		}
	}
	return lat, errs
}

// ---------- helpers ----------

func sec(d time.Duration) string { return fmt.Sprintf("%.1f с", d.Seconds()) }

func percentile(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(len(s)-1)*p/100]
}

func stats(d []time.Duration) string {
	if len(d) == 0 {
		return "нет данных"
	}
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	return fmt.Sprintf("мин %.2f с, сред %.2f с, p95 %.2f с, макс %.2f с",
		percentile(d, 0).Seconds(), (sum / time.Duration(len(d))).Seconds(), percentile(d, 95).Seconds(), percentile(d, 100).Seconds())
}

func errOr(err error, s string) string {
	if err != nil {
		return err.Error()
	}
	return s
}
