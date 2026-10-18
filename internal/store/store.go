package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arisros/tangkar/internal/order"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound = errors.New("tidak ditemukan")
	ErrClosed   = errors.New("pesanan sudah tutup")
	ErrSent     = errors.New("sudah terkirim ke penjual")
	ErrTooMany  = fmt.Errorf("maksimal %d mangkok per orang", order.MaxLines)
	ErrPast     = errors.New("jam sudah lewat")
	ErrPending  = errors.New("menunggu")
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	email      TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	seen_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	hash       TEXT PRIMARY KEY,
	email      TEXT NOT NULL REFERENCES users(email),
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS logins (
	device     TEXT PRIMARY KEY,
	user_code  TEXT NOT NULL UNIQUE,
	token      TEXT NOT NULL DEFAULT '',
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS orders (
	id         TEXT PRIMARY KEY,
	title      TEXT NOT NULL,
	lock_at    INTEGER NOT NULL,
	late_until INTEGER NOT NULL,
	status     TEXT NOT NULL,
	created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS orders_status ON orders(status);
CREATE TABLE IF NOT EXISTS lines (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	order_id   TEXT NOT NULL REFERENCES orders(id),
	email      TEXT NOT NULL,
	menu       TEXT NOT NULL CHECK (menu IN ('DAGING','CAMPUR')),
	sate       INTEGER NOT NULL CHECK (sate BETWEEN 0 AND 10),
	note       TEXT NOT NULL DEFAULT '' CHECK (length(note) <= 10),
	late       INTEGER NOT NULL,
	batch      INTEGER,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS lines_order ON lines(order_id, batch);
CREATE TABLE IF NOT EXISTS sends (
	order_id   TEXT NOT NULL REFERENCES orders(id),
	batch      INTEGER NOT NULL,
	status     TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT '',
	message    TEXT NOT NULL,
	sent_at    INTEGER NOT NULL DEFAULT 0,
	attempts   INTEGER NOT NULL DEFAULT 0,
	next_try   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (order_id, batch)
);
CREATE TABLE IF NOT EXISTS phones (
	email      TEXT PRIMARY KEY,
	msisdn     TEXT NOT NULL,
	source     TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS phone_codes (
	email      TEXT PRIMARY KEY,
	msisdn     TEXT NOT NULL,
	code_hash  TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	tries      INTEGER NOT NULL DEFAULT 0,
	sent_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notices (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	order_id   TEXT NOT NULL,
	batch      INTEGER NOT NULL,
	email      TEXT NOT NULL,
	msisdn     TEXT NOT NULL,
	message    TEXT NOT NULL,
	status     TEXT NOT NULL,
	error      TEXT NOT NULL DEFAULT '',
	attempts   INTEGER NOT NULL DEFAULT 0,
	next_try   INTEGER NOT NULL DEFAULT 0,
	UNIQUE (order_id, batch, email)
);
`

type Store struct {
	db  *sql.DB
	Now func() time.Time
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, Now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func randomID(n int, alphabet string) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

const slugChars = "abcdefghjkmnpqrstuvwxyz23456789"

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (s *Store) UpsertUser(ctx context.Context, email, name string) error {
	now := s.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(email,name,created_at,seen_at) VALUES(?,?,?,?)
		ON CONFLICT(email) DO UPDATE SET name=excluded.name, seen_at=excluded.seen_at`, email, name, now, now)
	return err
}

func (s *Store) UserName(ctx context.Context, email string) string {
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM users WHERE email=?`, email).Scan(&name); err != nil {
		return strings.SplitN(email, "@", 2)[0]
	}
	return name
}

// StartLogin begins the TUI's device login. The device code stays with the
// terminal; the short user code is what the person approves in a browser.
func (s *Store) StartLogin(ctx context.Context) (device, userCode string, err error) {
	device = randomID(32, slugChars)
	code := randomID(8, "ABCDEFGHJKLMNPQRSTUVWXYZ23456789")
	userCode = code[:4] + "-" + code[4:]
	now := s.Now().Unix()
	if _, err = s.db.ExecContext(ctx, `DELETE FROM logins WHERE expires_at<?`, now); err != nil {
		return "", "", err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO logins(device,user_code,expires_at) VALUES(?,?,?)`, hashToken(device), userCode, now+600)
	return device, userCode, err
}

func (s *Store) ApproveLogin(ctx context.Context, userCode, email string) error {
	token := "tgk_" + randomID(40, slugChars)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE logins SET token=? WHERE user_code=? AND token='' AND expires_at>=?`, token, strings.ToUpper(strings.TrimSpace(userCode)), s.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tokens(hash,email,created_at) VALUES(?,?,?)`, hashToken(token), email, s.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// PollLogin hands the token over once, then forgets the login.
func (s *Store) PollLogin(ctx context.Context, device string) (string, error) {
	var token string
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT token,expires_at FROM logins WHERE device=?`, hashToken(device)).Scan(&token, &exp)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && exp < s.Now().Unix()) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", ErrPending
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM logins WHERE device=?`, hashToken(device))
	return token, err
}

func (s *Store) TokenEmail(ctx context.Context, token string) (string, bool) {
	var email string
	err := s.db.QueryRowContext(ctx, `SELECT email FROM tokens WHERE hash=?`, hashToken(token)).Scan(&email)
	return email, err == nil
}

func (s *Store) RevokeToken(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE hash=?`, hashToken(token))
	return err
}

type Order struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	DayLabel  string    `json:"day_label"`
	LockAt    time.Time `json:"lock_at"`
	LateUntil time.Time `json:"late_until"`
	Status    string    `json:"status"`
	CreatedBy string    `json:"created_by"`
}

type Line struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Menu  string `json:"menu"`
	Sate  int    `json:"sate"`
	Note  string `json:"note"`
	Late  bool   `json:"late"`
	Batch *int   `json:"batch"`
	At    int64  `json:"at"`
}

func (l Line) Order() order.Line { return order.Line{Menu: l.Menu, Sate: l.Sate, Note: l.Note} }

type Send struct {
	Batch   int    `json:"batch"`
	Status  string `json:"status"`
	Error   string `json:"error"`
	Message string `json:"message"`
	SentAt  int64  `json:"sent_at"`
}

func (s *Store) CreateOrder(ctx context.Context, title string, lockAt time.Time, late time.Duration, by string) (Order, error) {
	if !lockAt.After(s.Now()) {
		return Order{}, ErrPast
	}
	id := randomID(6, slugChars)
	_, err := s.db.ExecContext(ctx, `INSERT INTO orders(id,title,lock_at,late_until,status,created_by,created_at) VALUES(?,?,?,?,?,?,?)`,
		id, title, lockAt.Unix(), lockAt.Add(late).Unix(), order.StatusOpen, by, s.Now().Unix())
	if err != nil {
		return Order{}, err
	}
	return s.Order(ctx, id)
}

const orderCols = `id,title,lock_at,late_until,status,created_by`

func scanOrder(sc interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var lockAt, lateUntil int64
	if err := sc.Scan(&o.ID, &o.Title, &lockAt, &lateUntil, &o.Status, &o.CreatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Order{}, ErrNotFound
		}
		return Order{}, err
	}
	o.LockAt = time.Unix(lockAt, 0)
	o.LateUntil = time.Unix(lateUntil, 0)
	o.DayLabel = order.DayLabel(o.LockAt)
	return o, nil
}

func (s *Store) Order(ctx context.Context, id string) (Order, error) {
	return scanOrder(s.db.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id=?`, id))
}

// Running is everything not finished plus whatever finished today, so the day's
// sends stay visible.
func (s *Store) Running(ctx context.Context) ([]Order, error) {
	d := s.Now().In(order.Jakarta)
	midnight := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, order.Jakarta).Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT `+orderCols+` FROM orders WHERE status IN (?,?) OR lock_at>=? ORDER BY lock_at DESC`,
		order.StatusOpen, order.StatusLate, midnight)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) Lines(ctx context.Context, orderID string) ([]Line, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.id,l.email,COALESCE(u.name,l.email),l.menu,l.sate,l.note,l.late,l.batch,l.created_at
		FROM lines l LEFT JOIN users u ON u.email=l.email WHERE l.order_id=? ORDER BY l.id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Line{}
	for rows.Next() {
		var l Line
		var batch sql.NullInt64
		if err := rows.Scan(&l.ID, &l.Email, &l.Name, &l.Menu, &l.Sate, &l.Note, &l.Late, &batch, &l.At); err != nil {
			return nil, err
		}
		if batch.Valid {
			b := int(batch.Int64)
			l.Batch = &b
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) Sends(ctx context.Context, orderID string) ([]Send, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT batch,status,error,message,sent_at FROM sends WHERE order_id=? ORDER BY batch`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Send{}
	for rows.Next() {
		var x Send
		if err := rows.Scan(&x.Batch, &x.Status, &x.Error, &x.Message, &x.SentAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// AddLine is the hardlock: the clock is checked inside the transaction, so a
// request that arrives at 11:30:00 lands as a late line or not at all.
func (s *Store) AddLine(ctx context.Context, orderID, email string, l order.Line) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id=?`, orderID))
	if err != nil {
		return err
	}
	now := s.Now()
	var late int
	var batch any
	switch {
	case o.Status == order.StatusOpen && now.Before(o.LockAt):
		batch = 0
	case (o.Status == order.StatusOpen || o.Status == order.StatusLate) && now.Before(o.LateUntil):
		late = 1
	default:
		return ErrClosed
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM lines WHERE order_id=? AND email=?`, orderID, email).Scan(&n); err != nil {
		return err
	}
	if n >= order.MaxLines {
		return ErrTooMany
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lines(order_id,email,menu,sate,note,late,batch,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		orderID, email, l.Menu, l.Sate, l.Note, late, batch, now.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteLine lets an owner take a bowl back until it has gone to the seller.
func (s *Store) DeleteLine(ctx context.Context, orderID string, lineID int64, email string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id=?`, orderID))
	if err != nil {
		return err
	}
	var late bool
	var batch sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT late,batch FROM lines WHERE id=? AND order_id=? AND email=?`, lineID, orderID, email).Scan(&late, &batch)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	now := s.Now()
	ok := (!late && o.Status == order.StatusOpen && now.Before(o.LockAt)) ||
		(late && !batch.Valid && o.Status != order.StatusCancelled)
	if !ok {
		return ErrSent
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM lines WHERE id=?`, lineID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetLockAt(ctx context.Context, orderID string, at time.Time) error {
	if !at.After(s.Now()) {
		return ErrPast
	}
	return s.exec1(ctx, `UPDATE orders SET late_until=late_until-lock_at+?, lock_at=? WHERE id=? AND status=?`, at.Unix(), at.Unix(), orderID, order.StatusOpen)
}

func (s *Store) LockNow(ctx context.Context, orderID string) error {
	now := s.Now().Unix()
	return s.exec1(ctx, `UPDATE orders SET late_until=late_until-lock_at+?, lock_at=? WHERE id=? AND status=? AND lock_at>?`, now, now, orderID, order.StatusOpen, now)
}

// SetLateUntil moves the late-join cutoff; a time at or before now closes late join.
func (s *Store) SetLateUntil(ctx context.Context, orderID string, at time.Time) error {
	return s.exec1(ctx, `UPDATE orders SET late_until=MAX(lock_at,?) WHERE id=? AND status IN (?,?)`, at.Unix(), orderID, order.StatusOpen, order.StatusLate)
}

func (s *Store) Cancel(ctx context.Context, orderID string) error {
	return s.exec1(ctx, `UPDATE orders SET status=? WHERE id=? AND status=? AND lock_at>?`, order.StatusCancelled, orderID, order.StatusOpen, s.Now().Unix())
}

func (s *Store) RetrySend(ctx context.Context, orderID string, batch int) error {
	return s.exec1(ctx, `UPDATE sends SET status=?, error='', attempts=0, next_try=0 WHERE order_id=? AND batch=? AND status=?`, order.SendPending, orderID, batch, order.SendFailed)
}

func (s *Store) exec1(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrClosed
	}
	return nil
}

// Tick advances every order whose clock has run: it locks, cuts late batches
// and closes, queueing a send for each message. It returns the orders it touched.
func (s *Store) Tick(ctx context.Context) ([]string, error) {
	now := s.Now()
	ids, err := s.ids(ctx, `SELECT id FROM orders WHERE status IN (?,?)`, order.StatusOpen, order.StatusLate)
	if err != nil {
		return nil, err
	}
	var touched []string
	for _, id := range ids {
		changed, err := s.tickOrder(ctx, id, now)
		if err != nil {
			return touched, fmt.Errorf("tick %s: %w", id, err)
		}
		if changed {
			touched = append(touched, id)
		}
	}
	return touched, nil
}

func (s *Store) tickOrder(ctx context.Context, id string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	o, err := scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM orders WHERE id=?`, id))
	if err != nil {
		return false, err
	}
	changed := false

	if o.Status == order.StatusOpen && !now.Before(o.LockAt) {
		lines, err := batchLines(ctx, tx, id, "batch=0")
		if err != nil {
			return false, err
		}
		status, msg := order.SendPending, order.Message(lines, 0)
		if len(lines) == 0 {
			status, msg = order.SendSkipped, ""
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sends(order_id,batch,status,message) VALUES(?,0,?,?)`, id, status, msg); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE orders SET status=? WHERE id=?`, order.StatusLate, id); err != nil {
			return false, err
		}
		o.Status = order.StatusLate
		changed = true
	}

	if o.Status == order.StatusLate {
		cut, err := s.cutLate(ctx, tx, o, now)
		if err != nil {
			return false, err
		}
		changed = changed || cut
		if !now.Before(o.LateUntil) {
			var waiting int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM lines WHERE order_id=? AND batch IS NULL`, id).Scan(&waiting); err != nil {
				return false, err
			}
			if waiting == 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE orders SET status=? WHERE id=?`, order.StatusClosed, id); err != nil {
					return false, err
				}
				changed = true
			}
		}
	}
	if !changed {
		return false, nil
	}
	return true, tx.Commit()
}

// cutLate turns waiting late lines into the next batch once nobody has added
// one for LateQuiet, the oldest has waited LateMaxWait, or late join has ended.
// The cut and the batch number are one transaction on the only connection, so
// a line inserted a moment later simply waits for the next batch. It holds back
// while an earlier message is unsent, so a TAMBAHAN never overtakes the order.
func (s *Store) cutLate(ctx context.Context, tx *sql.Tx, o Order, now time.Time) (bool, error) {
	var waiting int
	var first, last sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),MIN(created_at),MAX(created_at) FROM lines WHERE order_id=? AND batch IS NULL`, o.ID).Scan(&waiting, &first, &last); err != nil {
		return false, err
	}
	if waiting == 0 {
		return false, nil
	}
	if now.Before(CutAt(time.Unix(first.Int64, 0), time.Unix(last.Int64, 0), o.LateUntil)) {
		return false, nil
	}
	var unsent int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sends WHERE order_id=? AND status IN (?,?)`, o.ID, order.SendPending, order.SendFailed).Scan(&unsent); err != nil {
		return false, err
	}
	if unsent > 0 {
		return false, nil
	}
	var next, delivered int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(batch),0)+1, count(CASE WHEN status=? THEN 1 END) FROM sends WHERE order_id=?`, order.SendSent, o.ID).Scan(&next, &delivered); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE lines SET batch=? WHERE order_id=? AND batch IS NULL`, next, o.ID); err != nil {
		return false, err
	}
	lines, err := batchLines(ctx, tx, o.ID, fmt.Sprintf("batch=%d", next))
	if err != nil {
		return false, err
	}
	// The first delivered message was the full order, so the delivered count is
	// also this TAMBAHAN's number, and 0 means this one is the full order.
	msg := order.Message(lines, delivered)
	_, err = tx.ExecContext(ctx, `INSERT INTO sends(order_id,batch,status,message) VALUES(?,?,?,?)`, o.ID, next, order.SendPending, msg)
	return err == nil, err
}

// CutAt is when waiting late lines go to the seller.
func CutAt(first, last, lateUntil time.Time) time.Time {
	at := last.Add(order.LateQuiet)
	if cap := first.Add(order.LateMaxWait); cap.Before(at) {
		at = cap
	}
	if lateUntil.Before(at) {
		at = lateUntil
	}
	return at
}

type person struct {
	order.Line
	email, name string
}

func batchPeople(ctx context.Context, tx *sql.Tx, orderID, where string) ([]person, error) {
	rows, err := tx.QueryContext(ctx, `SELECT l.email,COALESCE(u.name,''),l.menu,l.sate,l.note FROM lines l
		LEFT JOIN users u ON u.email=l.email WHERE l.order_id=? AND `+where+` ORDER BY l.id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.email, &p.name, &p.Menu, &p.Sate, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// batchLines loads a batch with each bowl labelled by its owner's short name.
func batchLines(ctx context.Context, tx *sql.Tx, orderID, where string) ([]order.Line, error) {
	ps, err := batchPeople(ctx, tx, orderID, where)
	if err != nil {
		return nil, err
	}
	people := make([]order.Person, len(ps))
	for i, p := range ps {
		people[i] = order.Person{Email: p.email, Name: p.name}
	}
	short := order.ShortNames(people)
	out := make([]order.Line, len(ps))
	for i, p := range ps {
		out[i] = p.Line
		out[i].Who = short[p.email]
	}
	return out, nil
}

type Outbox struct {
	OrderID string
	Batch   int
	Message string
}

// MaxAttempts bounds automatic retries; after that a send waits for an admin.
const MaxAttempts = 5

func (s *Store) PendingSends(ctx context.Context) ([]Outbox, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT order_id,batch,message FROM sends
		WHERE status=? OR (status=? AND attempts<? AND next_try<=?) ORDER BY order_id,batch`,
		order.SendPending, order.SendFailed, MaxAttempts, s.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outbox
	for rows.Next() {
		var x Outbox
		if err := rows.Scan(&x.OrderID, &x.Batch, &x.Message); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// MarkSend records a delivery attempt. A delivered message also queues the
// personal notice for everyone in it who has a WhatsApp number.
func (s *Store) MarkSend(ctx context.Context, orderID string, batch int, sendErr error) error {
	if sendErr != nil {
		_, err := s.db.ExecContext(ctx, `UPDATE sends SET status=?, error=?, attempts=attempts+1, next_try=?+30*(attempts+1) WHERE order_id=? AND batch=?`,
			order.SendFailed, clip(sendErr.Error(), 300), s.Now().Unix(), orderID, batch)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE sends SET status=?, error='', sent_at=? WHERE order_id=? AND batch=?`, order.SendSent, s.Now().Unix(), orderID, batch); err != nil {
		return err
	}
	ps, err := batchPeople(ctx, tx, orderID, fmt.Sprintf("l.batch=%d", batch))
	if err != nil {
		return err
	}
	byEmail := map[string][]order.Line{}
	names := map[string]string{}
	var emails []string
	for _, p := range ps {
		if _, ok := byEmail[p.email]; !ok {
			emails = append(emails, p.email)
		}
		byEmail[p.email] = append(byEmail[p.email], p.Line)
		names[p.email] = p.name
	}
	for _, email := range emails {
		var msisdn string
		err := tx.QueryRowContext(ctx, `SELECT msisdn FROM phones WHERE email=?`, email).Scan(&msisdn)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		name := strings.Fields(names[email] + " " + strings.SplitN(email, "@", 2)[0])[0]
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notices(order_id,batch,email,msisdn,message,status) VALUES(?,?,?,?,?,?)`,
			orderID, batch, email, msisdn, order.Notice(name, byEmail[email]), order.SendPending); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Store) ids(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
