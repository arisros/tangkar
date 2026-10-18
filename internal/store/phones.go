package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/arisros/tangkar/internal/order"
)

var (
	ErrWait  = errors.New("tunggu 1 menit sebelum minta kode lagi")
	ErrCode  = errors.New("kode salah")
	ErrTries = errors.New("terlalu banyak salah, minta kode baru")
)

func (s *Store) Phone(ctx context.Context, email string) (string, bool) {
	var msisdn string
	err := s.db.QueryRowContext(ctx, `SELECT msisdn FROM phones WHERE email=?`, email).Scan(&msisdn)
	return msisdn, err == nil
}

func (s *Store) setPhone(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, email, msisdn, source string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO phones(email,msisdn,source,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(email) DO UPDATE SET msisdn=excluded.msisdn, source=excluded.source, updated_at=excluded.updated_at`,
		email, msisdn, source, s.Now().Unix())
	return err
}

func (s *Store) DeletePhone(ctx context.Context, email string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM phones WHERE email=?`, email)
	return err
}

// ImportPhones takes "email, number" lines (comma, semicolon, tab or space
// separated) and returns how many were saved and which lines were rejected.
func (s *Store) ImportPhones(ctx context.Context, text string) (int, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	var saved int
	var bad []string
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ';' || r == '\t' || r == ' ' })
		if len(f) < 2 {
			bad = append(bad, line)
			continue
		}
		email := strings.ToLower(f[0])
		msisdn, err := order.Phone(strings.Join(f[1:], ""))
		if !strings.Contains(email, "@") || err != nil {
			bad = append(bad, line)
			continue
		}
		if err := s.setPhone(ctx, tx, email, msisdn, "admin"); err != nil {
			return 0, nil, err
		}
		saved++
	}
	return saved, bad, tx.Commit()
}

// StartPhoneCode issues a 4-digit code for a number someone wants to add. The
// number only becomes theirs after VerifyPhoneCode, so nobody can register a
// colleague's WhatsApp.
func (s *Store) StartPhoneCode(ctx context.Context, email, msisdn string) (string, error) {
	now := s.Now().Unix()
	var sent int64
	err := s.db.QueryRowContext(ctx, `SELECT sent_at FROM phone_codes WHERE email=?`, email).Scan(&sent)
	if err == nil && now-sent < 60 {
		return "", ErrWait
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", err
	}
	code := fmt.Sprintf("%04d", n.Int64())
	_, err = s.db.ExecContext(ctx, `INSERT INTO phone_codes(email,msisdn,code_hash,expires_at,tries,sent_at) VALUES(?,?,?,?,0,?)
		ON CONFLICT(email) DO UPDATE SET msisdn=excluded.msisdn, code_hash=excluded.code_hash, expires_at=excluded.expires_at, tries=0, sent_at=excluded.sent_at`,
		email, msisdn, hashToken(email+"|"+code), now+600, now)
	return code, err
}

func (s *Store) VerifyPhoneCode(ctx context.Context, email, code string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var msisdn, hash string
	var exp int64
	var tries int
	err = tx.QueryRowContext(ctx, `SELECT msisdn,code_hash,expires_at,tries FROM phone_codes WHERE email=?`, email).Scan(&msisdn, &hash, &exp, &tries)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && exp < s.Now().Unix()) {
		return "", ErrTries
	}
	if err != nil {
		return "", err
	}
	if tries >= 5 {
		return "", ErrTries
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(email+"|"+strings.TrimSpace(code)))) != 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE phone_codes SET tries=tries+1 WHERE email=?`, email); err != nil {
			return "", err
		}
		return "", errors.Join(ErrCode, tx.Commit())
	}
	if err := s.setPhone(ctx, tx, email, msisdn, "self"); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM phone_codes WHERE email=?`, email); err != nil {
		return "", err
	}
	return msisdn, tx.Commit()
}

type Notice struct {
	ID      int64
	OrderID string
	To      string
	Message string
}

func (s *Store) PendingNotices(ctx context.Context) ([]Notice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,order_id,msisdn,message FROM notices
		WHERE status=? OR (status=? AND attempts<? AND next_try<=?) ORDER BY id`,
		order.SendPending, order.SendFailed, MaxAttempts, s.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		var n Notice
		if err := rows.Scan(&n.ID, &n.OrderID, &n.To, &n.Message); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) MarkNotice(ctx context.Context, id int64, sendErr error) error {
	if sendErr != nil {
		_, err := s.db.ExecContext(ctx, `UPDATE notices SET status=?, error=?, attempts=attempts+1, next_try=?+60*(attempts+1) WHERE id=?`,
			order.SendFailed, clip(sendErr.Error(), 300), s.Now().Unix(), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE notices SET status=?, error='' WHERE id=?`, order.SendSent, id)
	return err
}
