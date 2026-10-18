package order

import (
	"errors"
	"fmt"
	"math/rand/v2"
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
	LateMaxWait = 4 * time.Minute
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
	Who  string
	Menu string
	Sate int
	Note string
}

// String is the bowl as the seller reads it: "DAGING 2 SATE (no tauge)".
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

var hari = [...]string{"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu"}
var bulan = [...]string{"", "Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}

func DayLabel(t time.Time) string {
	t = t.In(Jakarta)
	return fmt.Sprintf("%s, %d %s %d", hari[t.Weekday()], t.Day(), bulan[t.Month()], t.Year())
}

// Pick chooses a template variant; tests replace it to get a fixed text.
var Pick = rand.IntN

var openers = []string{
	"Mang! Mau pesen tangkar ya 🍢🥩😋",
	"Halo Mang 👋 kita pesen soto tangkar lagi nih 😋🍲",
	"Mang, pesenan tangkar hari ini ya 🍲🔥",
	"Mang! Laper nih, pesen tangkar dong 🤤🍢",
	"Assalamualaikum Mang 🙏 mau pesen soto tangkar ya 🥩🍲",
	"Mang, tangkar lagi ya hari ini 😁🍢🔥",
	"Halo Mang! Kakak-kakak kantor mau tangkar nih 🍲😋",
	"Mang, siap-siap ya, pesenan tangkar dateng 🛵🍢🥩",
}

var extras = []string{
	"Mang, ada tambahan lagi nih 🙏🍢",
	"Mang, nyusul satu lagi ya 😅🍲",
	"Mang, maaf nambah lagi ya 🙏🥩",
	"Mang! Ketinggalan, tambah ya 😁🍢",
}

var closers = []string{
	"Makasih Mang 🙏",
	"Makasih banyak Mang 🙏😋",
	"Makasih ya Mang 🙏🔥",
}

// Message renders one send to the seller's group: a casual opener, one line
// per bowl with who it is for, and a thank-you. tambahan is 0 for the full
// order and n for TAMBAHAN #n.
func Message(lines []Line, tambahan int) string {
	var b strings.Builder
	if tambahan == 0 {
		b.WriteString(openers[Pick(len(openers))])
	} else {
		fmt.Fprintf(&b, "%s (TAMBAHAN #%d)", extras[Pick(len(extras))], tambahan)
	}
	b.WriteString("\n\n")
	for _, l := range lines {
		fmt.Fprintf(&b, "%s: %s\n", l.Who, l)
	}
	b.WriteString("\n" + closers[Pick(len(closers))])
	return b.String()
}

// Notice is the personal message someone gets once their bowls reached the seller.
func Notice(name string, bowls []Line) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Halo Kak %s 👋 orderan Kakak ke Mamang Tangkar sudah diproses ya:\n\n", name)
	for _, l := range bowls {
		b.WriteString("🍲 " + l.String() + "\n")
	}
	b.WriteString("\nSelamat makan nanti 😋🍢")
	return b.String()
}

type Person struct {
	Email string
	Name  string
}

// ShortNames gives everyone a short uppercase label for the seller: the first
// word of their Google name, or of their email when the name is empty. People
// who would share a label get the initial of their email's second part, and
// failing that their whole email name.
func ShortNames(people []Person) map[string]string {
	base := map[string]string{}
	count := map[string]int{}
	for _, p := range people {
		if _, ok := base[p.Email]; ok {
			continue
		}
		b := firstWord(p.Name)
		if b == "" {
			b = firstWord(emailParts(p.Email)[0])
		}
		base[p.Email] = b
		count[b]++
	}
	out := map[string]string{}
	for email, b := range base {
		if count[b] == 1 {
			out[email] = b
			continue
		}
		parts := emailParts(email)
		if len(parts) > 1 && parts[1] != "" {
			out[email] = b + " " + strings.ToUpper(string([]rune(parts[1])[:1]))
		} else {
			out[email] = strings.ToUpper(strings.SplitN(email, "@", 2)[0])
		}
	}
	return out
}

func firstWord(s string) string {
	f := strings.Fields(CleanText(s))
	if len(f) == 0 {
		return ""
	}
	return strings.ToUpper(f[0])
}

func emailParts(email string) []string {
	local := strings.SplitN(email, "@", 2)[0]
	return strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '_' || r == '-' || unicode.IsDigit(r) })
}

// Phone normalises an Indonesian WhatsApp number to 62XXXXXXXXX.
func Phone(s string) (string, error) {
	var d strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			d.WriteRune(r)
		}
	}
	n := d.String()
	switch {
	case strings.HasPrefix(n, "0"):
		n = "62" + n[1:]
	case strings.HasPrefix(n, "8"):
		n = "62" + n
	}
	if !strings.HasPrefix(n, "628") || len(n) < 10 || len(n) > 15 {
		return "", errors.New("nomor WhatsApp tidak valid, contoh 0812xxxxxxx")
	}
	return n, nil
}
