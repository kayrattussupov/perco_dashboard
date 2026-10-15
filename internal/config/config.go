// Package config loads perco-meal settings from config.yaml.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Meal binds a meal (screen key b/l/d) to a PERCo access template.
type Meal struct {
	Key        string `yaml:"key"`
	Name       string `yaml:"name"`
	TemplateID int    `yaml:"template_id"`
}

type Config struct {
	Perco struct {
		URL      string `yaml:"url"`
		Login    string `yaml:"login"`
		Password string `yaml:"password"`
	} `yaml:"perco"`
	Listen              string `yaml:"listen"`
	DB                  string `yaml:"db"`
	UTCOffsetHours      int    `yaml:"utc_offset_hours"`
	PollSeconds         int    `yaml:"poll_seconds"`
	StaffRefreshMinutes int    `yaml:"staff_refresh_minutes"`
	DeviceIDs           []int  `yaml:"device_ids"`
	Meals               []Meal `yaml:"meals"`
	PassEventIDs        []int  `yaml:"pass_event_ids"`
	DenyEventIDs        []int  `yaml:"deny_event_ids"`
	DoorEventIDs        []int  `yaml:"door_event_ids"`
	ResetMonitor        struct {
		Enabled         bool  `yaml:"enabled"`
		EventIDs        []int `yaml:"event_ids"`
		LookbackMinutes int   `yaml:"lookback_minutes"`
	} `yaml:"reset_monitor"`
	BackupReset bool      `yaml:"backup_reset"`
	Blocking    Blocking  `yaml:"blocking"`
	SelfCheck   SelfCheck `yaml:"selfcheck"`
}

// SelfCheck configures the daily health checks: the service verifies at these local times that the
// controller is connected and in sync, nobody is left blocked by mistake and no card went missing.
type SelfCheck struct {
	Enabled bool     `yaml:"enabled"`
	Times   []string `yaml:"times"` // "HH:MM" in the PERCo server time zone
}

// Blocking configures one-pass-per-meal enforcement by temporarily blocking employees in PERCo.
// The mode itself (off/test/on) is switched from the dashboard and stored in the database.
type Blocking struct {
	ForceOff    bool    `yaml:"force_off"`     // hard kill switch: never block, whatever the dashboard says
	AdminPIN    string  `yaml:"admin_pin"`     // required for mode changes and manual unblocks
	TestUserIDs []int64 `yaml:"test_user_ids"` // employees blocked in "test" mode
	MaxHours    float64 `yaml:"max_hours"`     // no block outlives this, even if a meal window is mis-read
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{
		Listen:              ":8080",
		DB:                  "perco-meal.db",
		UTCOffsetHours:      5,
		PollSeconds:         1,
		StaffRefreshMinutes: 30,
		PassEventIDs:        []int{17},
		DoorEventIDs:        []int{31},
	}
	c.ResetMonitor.LookbackMinutes = 90
	c.Blocking.MaxHours = 16
	c.SelfCheck.Enabled = true
	c.SelfCheck.Times = []string{"20:05", "06:30"}
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("config.yaml: %w", err)
	}
	if c.Perco.URL == "" || c.Perco.Login == "" {
		return nil, errors.New("config.yaml: perco.url и perco.login обязательны")
	}
	if len(c.Meals) == 0 {
		return nil, errors.New("config.yaml: не задан список meals")
	}
	for _, t := range c.SelfCheck.Times {
		if _, err := time.Parse("15:04", t); err != nil {
			return nil, fmt.Errorf("config.yaml: selfcheck.times: %q — нужен формат ЧЧ:ММ", t)
		}
	}
	if len(c.DeviceIDs) == 0 {
		return nil, errors.New("config.yaml: не задан device_ids")
	}
	return c, nil
}

// Location is the PERCo server time zone; event time labels are in it.
func (c *Config) Location() *time.Location {
	return time.FixedZone(fmt.Sprintf("UTC%+d", c.UTCOffsetHours), c.UTCOffsetHours*3600)
}
