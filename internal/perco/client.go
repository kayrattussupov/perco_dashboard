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
}

func New(baseURL, login, password string) *Client {
	return &Client{
		base:     strings.TrimRight(baseURL, "/") + "/api",
		login:    login,
		password: password,
		hc:       &http.Client{Timeout: 30 * time.Second},
	}
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
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("auth: HTTP %d: %w", resp.StatusCode, err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("auth: HTTP %d: %s", resp.StatusCode, out.Error)
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
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.dropToken()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s %s: HTTP %d: %.300s", method, path, resp.StatusCode, data)
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

// ResetAntipass sends "Сброс зональности" (cmdType 6) to a lock device.
func (c *Client) ResetAntipass(ctx context.Context, deviceID int) error {
	body := map[string]int{"cmdNumber": 1, "cmdType": 6, "cmdValue": 0}
	return c.do(ctx, http.MethodPost, "/devices/"+strconv.Itoa(deviceID)+"/command", nil, body, nil)
}
