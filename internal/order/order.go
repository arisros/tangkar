package order

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	Daging = "DAGING"
	Campur = "CAMPUR"

	MaxNote  = 10
	MaxSate  = 10
	MaxLines = 10

	DefaultLock = "11:30"
	DefaultLate = 60 * time.Minute
	LateQuiet   = 2 * time.Minute
)

const (
	StatusOpen      = "open"
	StatusLate      = "late"
	StatusClosed    = "closed"
	StatusCancelled = "cancelled"

	SendPending = "pending"
	SendSent    = "sent"
	SendFailed  = "failed"
	SendSkipped = "skipped"
)

var Jakarta = mustLoad("Asia/Jakarta")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

var (
	ErrMenu = errors.New("menu harus DAGING atau CAMPUR")
	ErrSate = fmt.Errorf("sate 0 sampai %d", MaxSate)
	ErrNote = fmt.Errorf("catatan maksimal %d huruf", MaxNote)
)

func ValidMenu(m string) (string, error) {
	m = strings.ToUpper(strings.TrimSpace(m))
	if m == Daging || m == Campur {
		return m, nil
	}
	return "", ErrMenu
}

func ValidSate(n int) error {
	if n < 0 || n > MaxSate {
		return ErrSate
	}
	return nil
}

func CleanNote(s string) (string, error) {
	out := CleanText(s)
	if utf8.RuneCountInString(out) > MaxNote {
		return "", ErrNote
	}
	return out, nil
}

// CleanText keeps letters, digits, spaces and a few harmless marks, so the
// result can never carry markup wherever it is shown.
func CleanText(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r), r == '.' || r == ',' || r == '-' || r == '+':
			b.WriteRune(r)
			space = false
		case unicode.IsSpace(r):
			if !space && b.Len() > 0 {
				b.WriteRune(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// ClockOn resolves "HH:MM" on the Jakarta day of t.
func ClockOn(t time.Time, hhmm string) (time.Time, error) {
	c, err := time.ParseInLocation("15:04", strings.TrimSpace(hhmm), Jakarta)
	if err != nil {
		return time.Time{}, errors.New("jam harus HH:MM")
	}
	d := t.In(Jakarta)
	return time.Date(d.Year(), d.Month(), d.Day(), c.Hour(), c.Minute(), 0, 0, Jakarta), nil
}

type Line struct {
	Menu string
	Sate int
	Note string
}

func (l Line) String() string {
	s := l.Menu
	if l.Sate > 0 {
		s += fmt.Sprintf(" %d SATE", l.Sate)
	}
	if l.Note != "" {
		s += " (" + l.Note + ")"
	}
	return s
}

type Totals struct {
	Bowls  int `json:"bowls"`
	Daging int `json:"daging"`
	Campur int `json:"campur"`
	Sate   int `json:"sate"`
}

func Sum(lines []Line) Totals {
	var t Totals
	for _, l := range lines {
		t.Bowls++
		t.Sate += l.Sate
		if l.Menu == Daging {
			t.Daging++
		} else {
			t.Campur++
		}
	}
	return t
}

func (t Totals) porsi() string {
	s := fmt.Sprintf("%d porsi", t.Bowls)
	if t.Sate > 0 {
		s += fmt.Sprintf(", %d sate", t.Sate)
	}
	return s
}

var hari = [...]string{"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu"}
var bulan = [...]string{"", "Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}

func DayLabel(t time.Time) string {
	t = t.In(Jakarta)
	return fmt.Sprintf("%s, %d %s %d", hari[t.Weekday()], t.Day(), bulan[t.Month()], t.Year())
}

// Message renders one send to the seller's group. With nothing sent before, it
// is the full order; otherwise it is TAMBAHAN #n carrying only the new bowls
// and the new grand total, so the seller never has to add anything up.
func Message(title string, lockAt time.Time, lines, before []Line, n int) string {
	var b strings.Builder
	if len(before) == 0 {
		fmt.Fprintf(&b, "*%s*\n%s\n\n", title, DayLabel(lockAt))
	} else {
		fmt.Fprintf(&b, "*TAMBAHAN #%d, %s*\n(di luar pesanan jam %s)\n\n", n, title, lockAt.In(Jakarta).Format("15:04"))
	}
	for _, l := range lines {
		b.WriteString(l.String())
		b.WriteByte('\n')
	}
	t := Sum(lines)
	if len(before) == 0 {
		fmt.Fprintf(&b, "\nTotal: %s\n(DAGING %d, CAMPUR %d)", t.porsi(), t.Daging, t.Campur)
		return b.String()
	}
	all := Sum(append(append([]Line{}, before...), lines...))
	fmt.Fprintf(&b, "\nTambahan: %s\nTotal semua: %s", t.porsi(), all.porsi())
	return b.String()
}
