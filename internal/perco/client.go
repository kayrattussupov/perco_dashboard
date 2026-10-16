// Package perco is a small read-mostly client for the PERCo-Web REST API.
package perco

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const TimeLayout = "2006-01-02 15:04:05"

type Client struct {
	base     string
	login    string
	password string
	hc       *http.Client

	mu    sync.Mutex
	token string

	sem        chan struct{}
	urgent     *bucket
	background *bucket
	br         breaker
	m          metrics
}

const requestTimeout = 15 * time.Second

func New(baseURL, login, password string) *Client {
	return &Client{
		base:       strings.TrimRight(baseURL, "/") + "/api",
		login:      login,
		password:   password,
		hc:         &http.Client{Timeout: requestTimeout},
		sem:        make(chan struct{}, maxInFlight),
		urgent:     newBucket(urgentEvery, urgentBurst),
		background: newBucket(backgroundEvery, backgroundBurst),
	}
}

// Stats reports this service's requests to PERCo over the last hour and the breaker state.
func (c *Client) Stats() Stats {
	now := time.Now()
	st := c.m.stats(now)
	c.br.mu.Lock()
	if c.br.failures >= breakAfter {
		st.BreakerOpen = true
		st.OpenUntil = c.br.openUntil.Format("15:04:05")
	}
	st.TimesOpened = c.br.opened
	c.br.mu.Unlock()
	return st
}

// send makes one HTTP request through the guard: budget (reads only), breaker, concurrency limit, metrics.
// It returns the body and status; a transport error, a timeout or HTTP 5xx counts as a failure of PERCo.
func (c *Client) send(ctx context.Context, req *http.Request, kind string) ([]byte, int, error) {
	if req.Method == http.MethodGet && !isUnbudgeted(ctx) {
		b := c.urgent
		if isBackground(ctx) {
			b = c.background
		}
		if err := b.wait(ctx); err != nil {
			return nil, 0, err
		}
	}
	if err := c.br.allow(time.Now()); err != nil {
		c.m.mu.Lock()
		c.m.paused++
		c.m.mu.Unlock()
		return nil, 0, err
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		c.br.release()
		return nil, 0, ctx.Err()
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	var data []byte
	status := 0
	if err == nil {
		status = resp.StatusCode
		data, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	<-c.sem
	end := time.Now()
	if err != nil && ctx.Err() != nil {
		c.br.release() // cancelled by us, not a failure of PERCo
		return nil, 0, err
	}
	failed := err != nil || status >= 500
	c.br.done(end, failed)
	c.m.add(sample{at: end, kind: kind, took: end.Sub(start), failed: failed})
	return data, status, err
}

// kindOf names a request for the metrics: numbers in the path are replaced by {id}.
func kindOf(method, path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if _, err := strconv.Atoi(p); err == nil && p != "" {
			parts[i] = "{id}"
		}
	}
	return method + " " + strings.Join(parts, "/")
}

type Event struct {
	ID          int64   `json:"id"`
	TimeLabel   string  `json:"time_label"`
	EventNameID int     `json:"event_name_id"`
	EventName   string  `json:"event_name"`
	FIO         *string `json:"fio"`
	UserID      *int64  `json:"user_id"`
	DeviceID    *int    `json:"device_id"`
	UserName    *string `json:"user_name"` // operator login for operator actions
}

type StaffBrief struct {
	ID           int64   `json:"id"`
	LastName     string  `json:"last_name"`
	FirstName    string  `json:"first_name"`
	MiddleName   *string `json:"middle_name"`
	DivisionName *string `json:"division_name"`
	PositionName *string `json:"position_name"`
	TabelNumber  *string `json:"tabel_number"`
	IsActive     int     `json:"is_active"`
}

func (s StaffBrief) FIO() string {
	parts := []string{s.LastName, s.FirstName}
	if s.MiddleName != nil {
		parts = append(parts, *s.MiddleName)
	}
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

type StaffDetail struct {
	ID             int64           `json:"id"`
	IsActive       int             `json:"is_active"`
	IsBlock        int             `json:"is_block"`
	AccessTemplate json.RawMessage `json:"access_template"`
	Photo          *string         `json:"photo"`
}

// TemplateIDs parses access_template, which PERCo returns as [{"3":"Завтрак"},…].
func (s StaffDetail) TemplateIDs() []int {
	var list []map[string]string
	if err := json.Unmarshal(s.AccessTemplate, &list); err != nil {
		return nil
	}
	var ids []int
	for _, m := range list {
		for k := range m {
			if id, err := strconv.Atoi(k); err == nil {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

type Interval struct {
	Begin int `json:"begin"`
	End   int `json:"end"`
}

func (c *Client) authenticate(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"login": c.login, "password": c.password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/system/auth", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	data, status, err := c.send(ctx, req, "POST /system/auth")
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("auth: HTTP %d: %w", status, err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("auth: HTTP %d: %s", status, out.Error)
	}
	return out.Token, nil
}

func (c *Client) currentToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		return c.token, nil
	}
	tok, err := c.authenticate(ctx)
	if err != nil {
		return "", err
	}
	c.token = tok
	return tok, nil
}

func (c *Client) dropToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// do performs a request, re-authenticating once on 401 (tokens expire when idle).
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.currentToken(ctx)
		if err != nil {
			return err
		}
		u := c.base + path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		data, status, err := c.send(ctx, req, kindOf(method, path))
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.dropToken()
			continue
		}
		if status != http.StatusOK {
			return fmt.Errorf("%s %s: HTTP %d: %.300s", method, path, status, data)
		}
		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("%s %s: %w", method, path, err)
			}
		}
		return nil
	}
	return errors.New("unauthorized")
}

// Events returns one page of the system event log, oldest id first.
func (c *Client) Events(ctx context.Context, from, to time.Time, page, rows int) ([]Event, error) {
	q := url.Values{
		"beginDatetime": {from.Format(TimeLayout)},
		"endDatetime":   {to.Format(TimeLayout)},
		"page":          {strconv.Itoa(page)},
		"rows":          {strconv.Itoa(rows)},
		"sidx":          {"id"},
		"sord":          {"asc"},
	}
	var out struct {
		Rows []Event `json:"rows"`
	}
	err := c.do(ctx, http.MethodGet, "/eventsystem", q, nil, &out)
	return out.Rows, err
}

func (c *Client) StaffList(ctx context.Context) ([]StaffBrief, error) {
	var out []StaffBrief
	err := c.do(ctx, http.MethodGet, "/users/staff/fullList", nil, nil, &out)
	return out, err
}

func (c *Client) Staff(ctx context.Context, id int64) (StaffDetail, error) {
	var out StaffDetail
	err := c.do(ctx, http.MethodGet, "/users/staff/"+strconv.FormatInt(id, 10), nil, nil, &out)
	return out, err
}

// TemplateSchedules returns the schedule (time criterion) ids used by an access template.
func (c *Client) TemplateSchedules(ctx context.Context, templateID int) ([]int, error) {
	var out struct {
		Access []struct {
			Rights struct {
				Schedule struct {
					ID int `json:"id"`
				} `json:"schedule"`
			} `json:"rights"`
		} `json:"access"`
	}
	if err := c.do(ctx, http.MethodGet, "/accessTemplates/"+strconv.Itoa(templateID), nil, nil, &out); err != nil {
		return nil, err
	}
	var ids []int
	for _, a := range out.Access {
		if a.Rights.Schedule.ID != 0 {
			ids = append(ids, a.Rights.Schedule.ID)
		}
	}
	return ids, nil
}

// ScheduleIntervals returns intervals (seconds from midnight) of a time-zone schedule.
func (c *Client) ScheduleIntervals(ctx context.Context, scheduleID int) ([]Interval, error) {
	var out struct {
		Intervals []struct {
			Intervals []Interval `json:"intervals"`
		} `json:"intervals"`
	}
	if err := c.do(ctx, http.MethodGet, "/accessSchedules/"+strconv.Itoa(scheduleID), nil, nil, &out); err != nil {
		return nil, err
	}
	var res []Interval
	for _, g := range out.Intervals {
		res = append(res, g.Intervals...)
	}
	return res, nil
}

// StaffBlockState reports whether an employee is blocked and still active.
func (c *Client) StaffBlockState(ctx context.Context, id int64) (blocked, active bool, err error) {
	d, err := c.Staff(ctx, id)
	return d.IsBlock == 1, d.IsActive == 1, err
}

// SetBlocked blocks or unblocks one employee via the partial mass-edit endpoint, which changes
// only is_block (verified on site 2026-10-12: card, templates and dates stay intact).
func (c *Client) SetBlocked(ctx context.Context, id int64, blocked bool) error {
	q := url.Values{"ids": {strconv.FormatInt(id, 10)}}
	var out struct {
		Result string `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/users/staff", q, map[string]bool{"is_block": blocked}, &out); err != nil {
		return err
	}
	if out.Result != "ok" {
		return fmt.Errorf("is_block=%v: ответ PERCo %q", blocked, out.Result)
	}
	return nil
}

// IDs encodings for the "ids" query parameter of the mass-edit endpoint. Which ones PERCo accepts for several
// ids is not verified yet: callers must read the result back.
const (
	IDsComma  = "comma"  // ids=1,2,3
	IDsJSON   = "json"   // ids=[1,2,3]
	IDsRepeat = "repeat" // ids=1&ids=2&ids=3
)

// SetBlockedMany blocks or unblocks several employees in one request.
func (c *Client) SetBlockedMany(ctx context.Context, ids []int64, blocked bool, format string) error {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = strconv.FormatInt(id, 10)
	}
	q := url.Values{}
	switch format {
	case IDsJSON:
		q.Set("ids", "["+strings.Join(strs, ",")+"]")
	case IDsRepeat:
		q["ids"] = strs
	default:
		q.Set("ids", strings.Join(strs, ","))
	}
	var out struct {
		Result string `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/users/staff", q, map[string]bool{"is_block": blocked}, &out); err != nil {
		return err
	}
	if out.Result != "ok" {
		return fmt.Errorf("is_block=%v: ответ PERCo %q", blocked, out.Result)
	}
	return nil
}

// BlockedPerson is an active employee whose is_block flag is set in PERCo.
type BlockedPerson struct {
	ID  int64
	FIO string
}

// BlockedStaff returns every active employee blocked in PERCo right now: one request to the staff table
// (about 0.3 s for 850 people) instead of one per employee.
func (c *Client) BlockedStaff(ctx context.Context) ([]BlockedPerson, error) {
	var out []BlockedPerson
	for page := 1; ; page++ {
		q := url.Values{"status": {"active"}, "rows": {"1000"}, "page": {strconv.Itoa(page)}, "sidx": {"id"}, "sord": {"asc"}}
		var res struct {
			Total int `json:"total"`
			Rows  []struct {
				ID      int64  `json:"id"`
				FIO     string `json:"fio"`
				IsBlock int    `json:"is_block"`
			} `json:"rows"`
		}
		if err := c.do(ctx, http.MethodGet, "/users/staff/table", q, nil, &res); err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			if r.IsBlock == 1 {
				out = append(out, BlockedPerson{ID: r.ID, FIO: strings.Join(strings.Fields(r.FIO), " ")})
			}
		}
		if page >= res.Total {
			return out, nil
		}
	}
}

// DeviceHealth is the link state between PERCo-Web and the controllers, as seen by /devices/state.
type DeviceHealth struct {
	Connected bool     // every controller reports connect_state == 1
	Pending   int      // commands PERCo still has to deliver (sum of cmd_count)
	Errors    []string // resources not transferred to the devices (/devices/{id}/errors)
	Message   string   // controller status text, for logs
}

// Settled means PERCo is on line with the controllers and has nothing left to deliver to them.
func (h DeviceHealth) Settled() bool { return h.Connected && h.Pending == 0 && len(h.Errors) == 0 }

// DeviceState reads only the controller link state and PERCo's command queue: one request. Errors stay empty.
func (c *Client) DeviceState(ctx context.Context) (DeviceHealth, error) {
	h, _, err := c.deviceState(ctx)
	return h, err
}

// DeviceHealth reads the link state and the not-yet-delivered resources of the given devices and of every device
// PERCo lists: 1 + N requests, for decisions that depend on delivery (settle check, scheduled checks).
func (c *Client) DeviceHealth(ctx context.Context, deviceIDs []int) (DeviceHealth, error) {
	h, listed, err := c.deviceState(ctx)
	if err != nil {
		return DeviceHealth{}, err
	}
	ids := map[int]bool{}
	for _, d := range append(slices.Clone(deviceIDs), listed...) {
		ids[d] = true
	}
	for id := range ids {
		var errs []struct {
			Name string `json:"name"`
		}
		if err := c.do(ctx, http.MethodGet, "/devices/"+strconv.Itoa(id)+"/errors", nil, nil, &errs); err != nil {
			return DeviceHealth{}, err
		}
		for _, e := range errs {
			h.Errors = append(h.Errors, e.Name)
		}
	}
	return h, nil
}

func (c *Client) deviceState(ctx context.Context) (DeviceHealth, []int, error) {
	var st struct {
		Items []struct {
			ID           int    `json:"id"`
			ConnectState int    `json:"connect_state"`
			CmdCount     int    `json:"cmd_count"`
			Message      string `json:"connect_message"`
		} `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/devices/state", nil, nil, &st); err != nil {
		return DeviceHealth{}, nil, err
	}
	h := DeviceHealth{Connected: len(st.Items) > 0}
	var ids []int
	for _, it := range st.Items {
		ids = append(ids, it.ID)
		h.Pending += it.CmdCount
		if it.ConnectState != 1 {
			h.Connected = false
		}
		if h.Message == "" || it.ConnectState != 1 {
			h.Message = it.Message
		}
	}
	return h, ids, nil
}

// ResetAntipass sends "Сброс зональности" (cmdType 6) to a lock device.
func (c *Client) ResetAntipass(ctx context.Context, deviceID int) error {
	body := map[string]int{"cmdNumber": 1, "cmdType": 6, "cmdValue": 0}
	return c.do(ctx, http.MethodPost, "/devices/"+strconv.Itoa(deviceID)+"/command", nil, body, nil)
}
