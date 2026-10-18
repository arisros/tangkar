package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arisros/tangkar/internal/order"
)

type clock struct{ t time.Time }

func (c *clock) at(hhmm string) {
	t, _ := order.ClockOn(c.t, hhmm)
	c.t = t
}

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Date(2026, 10, 19, 9, 0, 0, 0, order.Jakarta)}
	s.Now = func() time.Time { return c.t }
	return s, c
}

func mustOrder(t *testing.T, s *Store, c *clock) Order {
	t.Helper()
	lock, _ := order.ClockOn(c.t, "11:30")
	o, err := s.CreateOrder(context.Background(), "Soto Tangkar", lock, order.DefaultLate, "aris@bfi.co.id")
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func pending(t *testing.T, s *Store) []Outbox {
	t.Helper()
	p, err := s.PendingSends(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func tick(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	o := mustOrder(t, s, c)

	add := func(email string, l order.Line) error { return s.AddLine(ctx, o.ID, email, l) }
	if err := add("a@bfi.co.id", order.Line{Menu: order.Daging, Sate: 2}); err != nil {
		t.Fatal(err)
	}
	if err := add("b@bfi.co.id", order.Line{Menu: order.Campur, Sate: 1, Note: "no tauge"}); err != nil {
		t.Fatal(err)
	}

	c.at("11:29")
	tick(t, s)
	if len(pending(t, s)) != 0 {
		t.Fatal("sent before lock")
	}

	c.at("11:30")
	if err := add("c@bfi.co.id", order.Line{Menu: order.Daging}); err != nil {
		t.Fatal("late join at the lock second should be accepted as late:", err)
	}
	tick(t, s)
	p := pending(t, s)
	if len(p) != 1 || p[0].Batch != 0 || !strings.Contains(p[0].Message, "Total: 2 porsi, 3 sate") {
		t.Fatalf("first send wrong: %+v", p)
	}

	lines, _ := s.Lines(ctx, o.ID)
	first := lines[0]
	if err := s.DeleteLine(ctx, o.ID, first.ID, first.Email); !errors.Is(err, ErrSent) {
		t.Fatalf("deleted a locked line: %v", err)
	}

	c.at("11:33")
	tick(t, s)
	if len(pending(t, s)) != 1 {
		t.Fatal("late batch cut while the first message is still unsent")
	}
	if err := s.MarkSend(ctx, o.ID, 0, nil); err != nil {
		t.Fatal(err)
	}

	c.at("11:34")
	if err := add("d@bfi.co.id", order.Line{Menu: order.Campur, Sate: 2}); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	tick(t, s)
	if len(pending(t, s)) != 0 {
		t.Fatal("cut before the quiet period")
	}
	c.t = c.t.Add(time.Minute + time.Second)
	tick(t, s)
	p = pending(t, s)
	if len(p) != 1 || p[0].Batch != 1 {
		t.Fatalf("late batch missing: %+v", p)
	}
	want := "*TAMBAHAN #1, Soto Tangkar*\n(di luar pesanan jam 11:30)\n\nDAGING\nCAMPUR 2 SATE\n\nTambahan: 2 porsi, 2 sate\nTotal semua: 4 porsi, 5 sate"
	if p[0].Message != want {
		t.Fatalf("got:\n%s\nwant:\n%s", p[0].Message, want)
	}
	s.MarkSend(ctx, o.ID, 1, nil)

	c.at("12:00")
	if err := add("e@bfi.co.id", order.Line{Menu: order.Daging, Sate: 1}); err != nil {
		t.Fatal(err)
	}
	lines, _ = s.Lines(ctx, o.ID)
	last := lines[len(lines)-1]
	if err := s.DeleteLine(ctx, o.ID, last.ID, "x@bfi.co.id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else deleted a line: %v", err)
	}
	if err := s.DeleteLine(ctx, o.ID, last.ID, last.Email); err != nil {
		t.Fatalf("owner could not take back an unsent late line: %v", err)
	}
	add("e@bfi.co.id", order.Line{Menu: order.Daging, Sate: 1})

	c.at("12:30")
	if err := add("f@bfi.co.id", order.Line{Menu: order.Daging}); !errors.Is(err, ErrClosed) {
		t.Fatalf("added after late join ended: %v", err)
	}
	tick(t, s)
	p = pending(t, s)
	if len(p) != 1 || !strings.HasPrefix(p[0].Message, "*TAMBAHAN #2") {
		t.Fatalf("closing did not flush the waiting late line: %+v", p)
	}
	got, _ := s.Order(ctx, o.ID)
	if got.Status != order.StatusClosed {
		t.Fatalf("status %s, want closed", got.Status)
	}
}

func TestNobodyOnTimeThenLate(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	o := mustOrder(t, s, c)
	c.at("11:30")
	tick(t, s)
	sends, _ := s.Sends(ctx, o.ID)
	if len(sends) != 1 || sends[0].Status != order.SendSkipped {
		t.Fatalf("empty order should skip: %+v", sends)
	}
	c.at("11:40")
	s.AddLine(ctx, o.ID, "a@bfi.co.id", order.Line{Menu: order.Daging, Sate: 1})
	c.at("11:43")
	tick(t, s)
	p := pending(t, s)
	if len(p) != 1 || !strings.HasPrefix(p[0].Message, "*Soto Tangkar*") {
		t.Fatalf("first real message should be the full order: %+v", p)
	}
}

func TestLockNowAndLimits(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	o := mustOrder(t, s, c)
	for i := 0; i < order.MaxLines; i++ {
		if err := s.AddLine(ctx, o.ID, "a@bfi.co.id", order.Line{Menu: order.Daging}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddLine(ctx, o.ID, "a@bfi.co.id", order.Line{Menu: order.Daging}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("limit not enforced: %v", err)
	}
	if err := s.LockNow(ctx, o.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Order(ctx, o.ID)
	if !got.LockAt.Equal(c.t) || got.LateUntil.Sub(got.LockAt) != order.DefaultLate {
		t.Fatalf("lock now: %+v", got)
	}
	tick(t, s)
	if len(pending(t, s)) != 1 {
		t.Fatal("lock now did not send")
	}
}

func TestDeviceLogin(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	s.UpsertUser(ctx, "a@bfi.co.id", "A")
	dev, code, err := s.StartLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PollLogin(ctx, dev); !errors.Is(err, ErrPending) {
		t.Fatalf("want pending, got %v", err)
	}
	if err := s.ApproveLogin(ctx, strings.ToLower(code), "a@bfi.co.id"); err != nil {
		t.Fatal(err)
	}
	tok, err := s.PollLogin(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	if email, ok := s.TokenEmail(ctx, tok); !ok || email != "a@bfi.co.id" {
		t.Fatal("token does not resolve")
	}
	if _, err := s.PollLogin(ctx, dev); !errors.Is(err, ErrNotFound) {
		t.Fatal("token handed out twice")
	}
}

func TestSendRetry(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	o := mustOrder(t, s, c)
	s.AddLine(ctx, o.ID, "a@bfi.co.id", order.Line{Menu: order.Daging})
	c.at("11:30")
	tick(t, s)
	for i := 1; i <= MaxAttempts; i++ {
		if len(pending(t, s)) != 1 {
			t.Fatalf("attempt %d not offered", i)
		}
		s.MarkSend(ctx, o.ID, 0, errors.New("gateway down"))
		if len(pending(t, s)) != 0 {
			t.Fatal("retried without waiting")
		}
		c.t = c.t.Add(time.Duration(30*i) * time.Second)
	}
	if len(pending(t, s)) != 0 {
		t.Fatal("retried past the limit")
	}
	if err := s.RetrySend(ctx, o.ID, 0); err != nil {
		t.Fatal(err)
	}
	if len(pending(t, s)) != 1 {
		t.Fatal("admin retry not offered")
	}
}
