// Package store keeps a local SQLite copy of canteen events and staff.
package store

import (
	"database/sql"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS events (
	id        INTEGER PRIMARY KEY,
	ts        TEXT NOT NULL,
	day       TEXT NOT NULL,
	kind      TEXT NOT NULL,
	code      INTEGER NOT NULL,
	name      TEXT NOT NULL,
	user_id   INTEGER NOT NULL DEFAULT 0,
	fio       TEXT NOT NULL DEFAULT '',
	device_id INTEGER NOT NULL DEFAULT 0,
	meal      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_day ON events(day, ts);
CREATE TABLE IF NOT EXISTS staff (
	id        INTEGER PRIMARY KEY,
	fio       TEXT NOT NULL,
	division  TEXT NOT NULL DEFAULT '',
	position  TEXT NOT NULL DEFAULT '',
	tabel     TEXT NOT NULL DEFAULT '',
	templates TEXT NOT NULL DEFAULT '',
	photo     TEXT NOT NULL DEFAULT '',
	active    INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL);
` + blocksSchema

// migrations add columns to databases created by earlier versions; "duplicate column" errors are expected.
var migrations = []string{
	`ALTER TABLE events ADD COLUMN actor TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE events ADD COLUMN card TEXT NOT NULL DEFAULT ''`,
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// BackupTo writes a consistent copy of the database to path (which must not exist) while the service runs.
func (s *Store) BackupTo(path string) error {
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}

// Kinds of stored events.
const (
	KindPass  = "pass"
	KindDeny  = "deny"
	KindReset = "reset"
	KindDoor  = "door"
	KindOther = "other"
	// KindStaffOp is an operator's block/unblock of an employee (PERCo events 501404/501405).
	KindStaffOp = "staffop"
)

type Event struct {
	ID       int64  `json:"id"`
	TS       string `json:"ts"`
	Day      string `json:"-"`
	Kind     string `json:"kind"`
	Code     int    `json:"code"`
	Name     string `json:"name"`
	UserID   int64  `json:"user_id"`
	FIO      string `json:"fio"`
	DeviceID int    `json:"device_id"`
	Meal     string `json:"meal"`
	Actor    string `json:"-"`    // PERCo operator login for operator actions
	Card     string `json:"card"` // card number presented at the reader
}

// InsertEvent stores e unless already present; reports whether it was new.
// Rows stored before card numbers were kept get the number when the event is read again.
func (s *Store) InsertEvent(e Event) (bool, error) {
	r, err := s.db.Exec(`INSERT OR IGNORE INTO events(id,ts,day,kind,code,name,user_id,fio,device_id,meal,actor,card)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, e.TS, e.Day, e.Kind, e.Code, e.Name, e.UserID, e.FIO, e.DeviceID, e.Meal, e.Actor, e.Card)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	if n == 0 && e.Card != "" {
		_, err = s.db.Exec(`UPDATE events SET card=? WHERE id=? AND card=''`, e.Card, e.ID)
	}
	return n > 0, err
}

// MarkDeny reclassifies stored device events whose codes became known as refusals.
func (s *Store) MarkDeny(codes []int) error {
	if len(codes) == 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE events SET kind=? WHERE kind=? AND code IN (`+joinInts(codes)+`)`, KindDeny, KindOther)
	return err
}

// Events returns events for days in [fromDay, toDay], ordered by time.
func (s *Store) Events(fromDay, toDay string) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id,ts,day,kind,code,name,user_id,fio,device_id,meal,card
		FROM events WHERE day BETWEEN ? AND ? ORDER BY ts, id`, fromDay, toDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Day, &e.Kind, &e.Code, &e.Name, &e.UserID, &e.FIO, &e.DeviceID, &e.Meal, &e.Card); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) HasKindBetween(kind, fromTS, toTS string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind=? AND ts BETWEEN ? AND ?`, kind, fromTS, toTS).Scan(&n)
	return n > 0, err
}

type Staff struct {
	ID        int64  `json:"id"`
	FIO       string `json:"fio"`
	Division  string `json:"dep"`
	Position  string `json:"pos"`
	Tabel     string `json:"tab"`
	Templates []int  `json:"-"`
	HasPhoto  bool   `json:"photo"`
	Active    bool   `json:"-"`
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, x := range v {
		s[i] = strconv.Itoa(x)
	}
	return strings.Join(s, ",")
}

func splitInts(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if x, err := strconv.Atoi(p); err == nil {
			out = append(out, x)
		}
	}
	return out
}

func (s *Store) UpsertStaff(st Staff, photo string) error {
	_, err := s.db.Exec(`INSERT INTO staff(id,fio,division,position,tabel,templates,photo,active) VALUES(?,?,?,?,?,?,?,1)
		ON CONFLICT(id) DO UPDATE SET fio=excluded.fio, division=excluded.division, position=excluded.position,
		tabel=excluded.tabel, templates=excluded.templates, photo=excluded.photo, active=1`,
		st.ID, st.FIO, st.Division, st.Position, st.Tabel, joinInts(st.Templates), photo)
	return err
}

// DeactivateExcept marks staff missing from the latest PERCo list as inactive.
func (s *Store) DeactivateExcept(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS keep(id INTEGER PRIMARY KEY); DELETE FROM keep;`); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO keep(id) VALUES(?)`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE staff SET active = (id IN (SELECT id FROM keep))`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AllStaff() ([]Staff, error) {
	rows, err := s.db.Query(`SELECT id,fio,division,position,tabel,templates,photo<>'',active FROM staff ORDER BY fio`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Staff
	for rows.Next() {
		var st Staff
		var tpl string
		if err := rows.Scan(&st.ID, &st.FIO, &st.Division, &st.Position, &st.Tabel, &tpl, &st.HasPhoto, &st.Active); err != nil {
			return nil, err
		}
		st.Templates = splitInts(tpl)
		out = append(out, st)
	}
	return out, rows.Err()
}

// StaffTemplates returns cached access template ids of an employee (nil if unknown).
func (s *Store) StaffTemplates(id int64) []int {
	var tpl string
	if s.db.QueryRow(`SELECT templates FROM staff WHERE id=?`, id).Scan(&tpl) != nil {
		return nil
	}
	return splitInts(tpl)
}

func (s *Store) StaffKnown(id int64) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM staff WHERE id=?`, id).Scan(&n)
	return n > 0
}

func (s *Store) Photo(id int64) (string, error) {
	var p string
	err := s.db.QueryRow(`SELECT photo FROM staff WHERE id=?`, id).Scan(&p)
	return p, err
}

func (s *Store) GetKV(k string) string {
	var v string
	s.db.QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	return v
}

func (s *Store) SetKV(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}
