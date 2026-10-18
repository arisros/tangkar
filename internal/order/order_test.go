package order

import (
	"testing"
	"time"
)

func TestCleanNote(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{"tanpa tauge", "", true},
		{"no tauge", "no tauge", false},
		{"<b>pedes</b>", "bpedesb", false},
		{"<script>", "script", false},
		{`"'><img`, "img", false},
		{"  kuah   pisah ", "kuah pisah", false},
		{"nasi\n\tdua", "nasi dua", false},
		{"pedas!!!😀", "pedas", false},
		{"sambel+", "sambel+", false},
		{"12345678901", "", true},
		{"", "", false},
	}
	for _, c := range cases {
		got, err := CleanNote(c.in)
		if c.err {
			if err == nil {
				t.Errorf("CleanNote(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("CleanNote(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestValid(t *testing.T) {
	if m, err := ValidMenu(" daging "); err != nil || m != Daging {
		t.Fatalf("got %q %v", m, err)
	}
	if _, err := ValidMenu("SATE"); err == nil {
		t.Fatal("SATE accepted as menu")
	}
	if ValidSate(-1) == nil || ValidSate(11) == nil || ValidSate(0) != nil {
		t.Fatal("sate bounds")
	}
}

func TestClockOn(t *testing.T) {
	got, err := ClockOn(time.Date(2026, 10, 19, 3, 0, 0, 0, time.UTC), "11:30")
	if err != nil || got.Format("2006-01-02 15:04 MST") != "2026-10-19 11:30 WIB" {
		t.Fatalf("got %s %v", got, err)
	}
	if _, err := ClockOn(time.Now(), "25:99"); err == nil {
		t.Fatal("bad time accepted")
	}
}

var lock = time.Date(2026, 10, 19, 11, 30, 0, 0, Jakarta)

func TestMessageFirst(t *testing.T) {
	got := Message("Soto Tangkar", lock, []Line{
		{Daging, 2, ""}, {Daging, 2, "no tauge"}, {Campur, 1, ""}, {Daging, 3, ""}, {Campur, 0, ""},
	}, nil, 0)
	want := `*Soto Tangkar*
Senin, 19 Oktober 2026

DAGING 2 SATE
DAGING 2 SATE (no tauge)
CAMPUR 1 SATE
DAGING 3 SATE
CAMPUR

Total: 5 porsi, 8 sate
(DAGING 3, CAMPUR 2)`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestMessageLate(t *testing.T) {
	before := []Line{{Daging, 2, ""}, {Daging, 2, ""}, {Campur, 1, ""}, {Daging, 3, ""}, {Campur, 0, ""}}
	got := Message("Soto Tangkar", lock, []Line{{Campur, 2, ""}, {Daging, 0, ""}}, before, 1)
	want := `*TAMBAHAN #1, Soto Tangkar*
(di luar pesanan jam 11:30)

CAMPUR 2 SATE
DAGING

Tambahan: 2 porsi, 2 sate
Total semua: 7 porsi, 10 sate`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
