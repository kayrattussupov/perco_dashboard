package store

import (
	"database/sql"
	"errors"
)

const blocksSchema = `
CREATE TABLE IF NOT EXISTS blocks (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id       INTEGER NOT NULL,
	fio           TEXT NOT NULL DEFAULT '',
	day           TEXT NOT NULL,
	meal          TEXT NOT NULL,
	state         TEXT NOT NULL,
	pass_event_id INTEGER NOT NULL DEFAULT 0,
	created_at    TEXT NOT NULL,
	blocked_at    TEXT NOT NULL DEFAULT '',
	restore_at    TEXT NOT NULL,
	deadline      TEXT NOT NULL,
	closed_at     TEXT NOT NULL DEFAULT '',
	note          TEXT NOT NULL DEFAULT '',
	attempts      INTEGER NOT NULL DEFAULT 0,
	next_try      TEXT NOT NULL DEFAULT '',
	last_error    TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS blocks_once ON blocks(user_id, day, meal);
CREATE INDEX IF NOT EXISTS blocks_state ON blocks(state);
CREATE TABLE IF NOT EXISTS own_writes (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	ts      TEXT NOT NULL,
	user_id INTEGER NOT NULL,
	blocked INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS own_writes_user ON own_writes(user_id, ts);
CREATE TABLE IF NOT EXISTS audit (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	ts     TEXT NOT NULL,
	actor  TEXT NOT NULL,
	action TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT ''
);
`

// Block states. A row is created before PERCo is touched (write-ahead), so a crash between the
// two is recovered on the next start.
const (
	BlockPending  = "pending"  // intent recorded, PERCo block not confirmed yet
	BlockActive   = "blocked"  // service blocked the employee and owes an unblock
	BlockHeld     = "operator" // an operator blocked on top: the service never unblocks it
	BlockReleased = "released" // closed: service unblocked
	BlockClosed   = "closed"   // closed without service unblock (operator unblocked, already unblocked, skipped, dismissed)
)

type Block struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"user_id"`
	FIO         string `json:"fio"`
	Day         string `json:"day"`
	Meal        string `json:"meal"`
	State       string `json:"state"`
	PassEventID int64  `json:"-"`
	CreatedAt   string `json:"created_at"`
	BlockedAt   string `json:"blocked_at"`
	RestoreAt   string `json:"restore_at"`
	Deadline    string `json:"deadline"`
	ClosedAt    string `json:"closed_at"`
	Note        string `json:"note"`
	Attempts    int    `json:"attempts"`
	NextTry     string `json:"-"`
	LastError   string `json:"last_error"`
}

// ErrBlockExists means this employee already has a block row for this meal today.
var ErrBlockExists = errors.New("block already recorded for this meal")

// CreateBlock records the intent to block; at most one row per (user, day, meal).
func (s *Store) CreateBlock(b Block) (int64, error) {
	r, err := s.db.Exec(`INSERT OR IGNORE INTO blocks(user_id,fio,day,meal,state,pass_event_id,created_at,restore_at,deadline)
		VALUES(?,?,?,?,?,?,?,?,?)`, b.UserID, b.FIO, b.Day, b.Meal, b.State, b.PassEventID, b.CreatedAt, b.RestoreAt, b.Deadline)
	if err != nil {
		return 0, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return 0, ErrBlockExists
	}
	return r.LastInsertId()
}

// UpdateBlock persists the mutable fields of b.
func (s *Store) UpdateBlock(b Block) error {
	_, err := s.db.Exec(`UPDATE blocks SET state=?, blocked_at=?, restore_at=?, closed_at=?, note=?, attempts=?, next_try=?, last_error=?
		WHERE id=?`, b.State, b.BlockedAt, b.RestoreAt, b.ClosedAt, b.Note, b.Attempts, b.NextTry, b.LastError, b.ID)
	return err
}

const blockCols = `id,user_id,fio,day,meal,state,pass_event_id,created_at,blocked_at,restore_at,deadline,closed_at,note,attempts,next_try,last_error`

func scanBlocks(rows *sql.Rows, err error) ([]Block, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Block
	for rows.Next() {
		var b Block
		if err := rows.Scan(&b.ID, &b.UserID, &b.FIO, &b.Day, &b.Meal, &b.State, &b.PassEventID, &b.CreatedAt, &b.BlockedAt,
			&b.RestoreAt, &b.Deadline, &b.ClosedAt, &b.Note, &b.Attempts, &b.NextTry, &b.LastError); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// OpenBlocks returns rows the service still watches, oldest first.
func (s *Store) OpenBlocks() ([]Block, error) {
	return scanBlocks(s.db.Query(`SELECT ` + blockCols + ` FROM blocks WHERE state IN ('pending','blocked','operator') ORDER BY id`))
}

func (s *Store) BlockByID(id int64) (Block, error) {
	bs, err := scanBlocks(s.db.Query(`SELECT `+blockCols+` FROM blocks WHERE id=?`, id))
	if err != nil {
		return Block{}, err
	}
	if len(bs) == 0 {
		return Block{}, sql.ErrNoRows
	}
	return bs[0], nil
}

// BlocksOfDay returns all rows of a day, newest first.
func (s *Store) BlocksOfDay(day string) ([]Block, error) {
	return scanBlocks(s.db.Query(`SELECT `+blockCols+` FROM blocks WHERE day=? ORDER BY id DESC`, day))
}

// OperatorAction is a block/unblock of an employee done in PERCo.
type OperatorAction struct {
	Code  int
	TS    string
	Actor string
}

// OperatorActions returns block/unblock events for a user at or after sinceTS done by anyone but self.
func (s *Store) OperatorActions(userID int64, sinceTS, self string) ([]OperatorAction, error) {
	rows, err := s.db.Query(`SELECT code, ts, actor FROM events WHERE kind=? AND user_id=? AND ts>=? AND actor<>? ORDER BY ts, id`,
		KindStaffOp, userID, sinceTS, self)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OperatorAction
	for rows.Next() {
		var a OperatorAction
		if err := rows.Scan(&a.Code, &a.TS, &a.Actor); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecordWrite notes, before the request, that this service is about to set is_block for an employee.
func (s *Store) RecordWrite(ts string, userID int64, blocked bool) error {
	_, err := s.db.Exec(`INSERT INTO own_writes(ts,user_id,blocked) VALUES(?,?,?)`, ts, userID, blocked)
	return err
}

// HasWrite reports whether this service recorded such a write between fromTS and toTS.
func (s *Store) HasWrite(userID int64, blocked bool, fromTS, toTS string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM own_writes WHERE user_id=? AND blocked=? AND ts>=? AND ts<=?`,
		userID, blocked, fromTS, toTS).Scan(&n)
	return n > 0, err
}

func (s *Store) Audit(ts, actor, action, detail string) error {
	_, err := s.db.Exec(`INSERT INTO audit(ts,actor,action,detail) VALUES(?,?,?,?)`, ts, actor, action, detail)
	return err
}
