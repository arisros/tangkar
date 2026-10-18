package store

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
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
	order.Pick = func(int) int { return 0 }
	t.Cleanup(func() { order.Pick = rand.IntN })
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
	if len(p) != 1 || p[0].Batch != 0 || p[0].Message != "Mang! Mau pesen tangkar ya 🍢🥩😋\n\nA: DAGING 2 SATE\nB: CAMPUR 1 SATE (no tauge)\n\nMakasih Mang 🙏" {
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
	p = pending(t, s)
	if len(p) != 1 || p[0].Batch != 1 {
		t.Fatalf("C waited since 11:30, past the 4 minute cap: %+v", p)
	}
	want := "Mang, ada tambahan lagi nih 🙏🍢 (TAMBAHAN #1)\n\nC: DAGING\nD: CAMPUR 2 SATE\n\nMakasih Mang 🙏"
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
	if len(p) != 1 || !strings.Contains(p[0].Message, "(TAMBAHAN #2)") {
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
	if len(p) != 1 || !strings.HasPrefix(p[0].Message, "Mang! Mau pesen") {
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

// lockedAndSent leaves an order in late join with its first message delivered.
func lockedAndSent(t *testing.T, s *Store, c *clock) Order {
	t.Helper()
	ctx := context.Background()
	o := mustOrder(t, s, c)
	s.AddLine(ctx, o.ID, "a@bfi.co.id", order.Line{Menu: order.Daging})
	c.at("11:30")
	tick(t, s)
	if err := s.MarkSend(ctx, o.ID, 0, nil); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestLateDebounce(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	o := lockedAndSent(t, s, c)
	c.at("11:40")
	start := c.t
	for _, d := range []time.Duration{0, 90 * time.Second, 3 * time.Minute} {
		c.t = start.Add(d)
		s.AddLine(ctx, o.ID, "b@bfi.co.id", order.Line{Menu: order.Campur})
		tick(t, s)
		if len(pending(t, s)) != 0 {
			t.Fatalf("cut at +%s while people were still joining", d)
		}
	}
	c.t = start.Add(4*time.Minute - time.Second)
	tick(t, s)
	if len(pending(t, s)) != 0 {
		t.Fatal("cut before the cap")
	}
	c.t = start.Add(4 * time.Minute)
	tick(t, s)
	if p := pending(t, s); len(p) != 1 || strings.Count(p[0].Message, "B: CAMPUR") != 3 {
		t.Fatalf("cap did not send all three: %+v", p)
	}
}

// TestCutRace orders in the same second as the cut: the bowl must land in
// exactly one batch, and the one already cut can no longer be taken back.
func TestCutRace(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	o := lockedAndSent(t, s, c)
	c.at("11:40")
	s.AddLine(ctx, o.ID, "b@bfi.co.id", order.Line{Menu: order.Campur})
	c.t = c.t.Add(order.LateQuiet)
	tick(t, s)
	if err := s.AddLine(ctx, o.ID, "c@bfi.co.id", order.Line{Menu: order.Daging, Sate: 1}); err != nil {
		t.Fatal(err)
	}
	tick(t, s)
	lines, _ := s.Lines(ctx, o.ID)
	b, cl := lines[1], lines[2]
	if b.Batch == nil || *b.Batch != 1 || cl.Batch != nil {
		t.Fatalf("batches wrong: b=%v c=%v", b.Batch, cl.Batch)
	}
	if err := s.DeleteLine(ctx, o.ID, b.ID, b.Email); !errors.Is(err, ErrSent) {
		t.Fatalf("deleted a bowl already cut for the seller: %v", err)
	}
	if p := pending(t, s); len(p) != 1 || strings.Contains(p[0].Message, "C: DAGING") {
		t.Fatalf("late bowl leaked into the cut batch: %+v", p)
	}
}

func TestNotices(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	s.UpsertUser(ctx, "denis.saputra@bfi.co.id", "Denis")
	n, bad, err := s.ImportPhones(ctx, "denis.saputra@bfi.co.id, 0812-3456-789\n# comment\nbroken line\nx@bfi.co.id 021")
	if err != nil || n != 1 || len(bad) != 2 {
		t.Fatalf("import: %d %v %v", n, bad, err)
	}
	o := mustOrder(t, s, c)
	s.AddLine(ctx, o.ID, "denis.saputra@bfi.co.id", order.Line{Menu: order.Daging, Sate: 1, Note: "no tauge"})
	s.AddLine(ctx, o.ID, "denis.saputra@bfi.co.id", order.Line{Menu: order.Campur})
	s.AddLine(ctx, o.ID, "nophone@bfi.co.id", order.Line{Menu: order.Campur})
	c.at("11:30")
	tick(t, s)
	if p := pending(t, s); len(p) != 1 || !strings.Contains(p[0].Message, "DENIS: DAGING 1 SATE (no tauge)") || !strings.Contains(p[0].Message, "NOPHONE: CAMPUR") {
		t.Fatalf("names in message: %+v", p)
	}
	if ns, _ := s.PendingNotices(ctx); len(ns) != 0 {
		t.Fatal("notice before the seller got the order")
	}
	s.MarkSend(ctx, o.ID, 0, nil)
	s.MarkSend(ctx, o.ID, 0, nil)
	ns, _ := s.PendingNotices(ctx)
	if len(ns) != 1 || ns[0].To != "628123456789" || ns[0].Message != order.Notice("Denis", []order.Line{{Menu: order.Daging, Sate: 1, Note: "no tauge"}, {Menu: order.Campur}}) {
		t.Fatalf("notices: %+v", ns)
	}
}

func TestPhoneCode(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	code, err := s.StartPhoneCode(ctx, "a@bfi.co.id", "628123456789")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartPhoneCode(ctx, "a@bfi.co.id", "628123456789"); !errors.Is(err, ErrWait) {
		t.Fatal("code resent without waiting")
	}
	if _, err := s.VerifyPhoneCode(ctx, "b@bfi.co.id", code); err == nil {
		t.Fatal("someone else's code accepted")
	}
	wrong := "0000"
	if code == wrong {
		wrong = "1111"
	}
	for i := 0; i < 5; i++ {
		s.VerifyPhoneCode(ctx, "a@bfi.co.id", wrong)
	}
	if _, err := s.VerifyPhoneCode(ctx, "a@bfi.co.id", code); !errors.Is(err, ErrTries) {
		t.Fatalf("right code accepted after 5 wrong tries: %v", err)
	}
	c.t = c.t.Add(time.Minute)
	code, _ = s.StartPhoneCode(ctx, "a@bfi.co.id", "628123456789")
	if got, err := s.VerifyPhoneCode(ctx, "a@bfi.co.id", code); err != nil || got != "628123456789" {
		t.Fatalf("verify: %q %v", got, err)
	}
	if p, ok := s.Phone(ctx, "a@bfi.co.id"); !ok || p != "628123456789" {
		t.Fatal("phone not saved")
	}
}

func TestUpdateLine(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	o := mustOrder(t, s, c)
	s.AddLine(ctx, o.ID, "a@bfi.co.id", order.Line{Menu: order.Daging, Sate: 2})
	lines, _ := s.Lines(ctx, o.ID)
	id := lines[0].ID
	if err := s.UpdateLine(ctx, o.ID, id, "b@bfi.co.id", order.Line{Menu: order.Campur}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else changed a bowl: %v", err)
	}
	if err := s.UpdateLine(ctx, o.ID, id, "a@bfi.co.id", order.Line{Menu: order.Campur, Sate: 1, Note: "pedes"}); err != nil {
		t.Fatal(err)
	}
	lines, _ = s.Lines(ctx, o.ID)
	if l := lines[0]; l.Menu != order.Campur || l.Sate != 1 || l.Note != "pedes" || len(lines) != 1 {
		t.Fatalf("not changed in place: %+v", lines)
	}
	c.at("11:30")
	if err := s.UpdateLine(ctx, o.ID, id, "a@bfi.co.id", order.Line{Menu: order.Daging}); !errors.Is(err, ErrSent) {
		t.Fatalf("changed after the lock: %v", err)
	}
}

func TestUpdateLateLineRestartsDebounce(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	o := lockedAndSent(t, s, c)
	c.at("11:40")
	s.AddLine(ctx, o.ID, "b@bfi.co.id", order.Line{Menu: order.Campur})
	lines, _ := s.Lines(ctx, o.ID)
	late := lines[len(lines)-1]
	c.t = c.t.Add(90 * time.Second)
	if err := s.UpdateLine(ctx, o.ID, late.ID, late.Email, order.Line{Menu: order.Daging, Sate: 3}); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(90 * time.Second)
	tick(t, s)
	if len(pending(t, s)) != 0 {
		t.Fatal("cut 90s after a change; the change should restart the quiet period")
	}
	c.t = c.t.Add(31 * time.Second)
	tick(t, s)
	if p := pending(t, s); len(p) != 1 || !strings.Contains(p[0].Message, "B: DAGING 3 SATE") {
		t.Fatalf("changed bowl not sent: %+v", p)
	}
}

func TestFriendOrder(t *testing.T) {
	ctx := context.Background()
	s, c := newStore(t)
	s.UpsertUser(ctx, "aris.kurniawan@bfi.co.id", "Aris")
	s.ImportPhones(ctx, "aris.kurniawan@bfi.co.id, 08123456789")
	o := mustOrder(t, s, c)
	s.AddLine(ctx, o.ID, "aris.kurniawan@bfi.co.id", order.Line{Menu: order.Campur, Sate: 2})
	if err := s.AddLine(ctx, o.ID, "aris.kurniawan@bfi.co.id", order.Line{Menu: order.Daging, Sate: 1, For: "Okky"}); err != nil {
		t.Fatal(err)
	}
	lines, _ := s.Lines(ctx, o.ID)
	friend := lines[1]
	if friend.For != "Okky" || friend.Email != "aris.kurniawan@bfi.co.id" {
		t.Fatalf("friend line: %+v", friend)
	}
	if err := s.DeleteLine(ctx, o.ID, friend.ID, "okky@bfi.co.id"); !errors.Is(err, ErrNotFound) {
		t.Fatal("only the orderer may touch a friend's bowl")
	}
	c.at("11:30")
	tick(t, s)
	p := pending(t, s)
	if len(p) != 1 || !strings.Contains(p[0].Message, "ARIS: CAMPUR 2 SATE\nOKKY: DAGING 1 SATE") || strings.Contains(p[0].Message, "via") {
		t.Fatalf("seller message: %+v", p)
	}
	s.MarkSend(ctx, o.ID, 0, nil)
	ns, _ := s.PendingNotices(ctx)
	if len(ns) != 1 || !strings.Contains(ns[0].Message, "🍲 DAGING 1 SATE untuk Okky") {
		t.Fatalf("orderer notice: %+v", ns)
	}
}

func TestMigrateOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE lines (id INTEGER PRIMARY KEY AUTOINCREMENT, order_id TEXT NOT NULL, email TEXT NOT NULL,
		menu TEXT NOT NULL, sate INTEGER NOT NULL, note TEXT NOT NULL DEFAULT '', late INTEGER NOT NULL, batch INTEGER, created_at INTEGER NOT NULL);
		INSERT INTO lines(order_id,email,menu,sate,note,late,batch,created_at) VALUES('x','a@bfi.co.id','DAGING',1,'',0,0,1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lines, err := s.Lines(context.Background(), "x")
	if err != nil || len(lines) != 1 || lines[0].For != "" {
		t.Fatalf("old bowl after migration: %+v %v", lines, err)
	}
	if _, err := Open(path); err != nil {
		t.Fatalf("second open: %v", err)
	}
}
