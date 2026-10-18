package order

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestCleanNote(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{"tanpa tauge", "tanpa tauge", false},
		{"tanpa daun bawang ya", "tanpa daun bawang ya", false},
		{"tanpa daun bawang yaa", "", true},
		{"no tauge", "no tauge", false},
		{"<b>pedes</b>", "bpedesb", false},
		{"<script>", "script", false},
		{`"'><img`, "img", false},
		{"  kuah   pisah ", "kuah pisah", false},
		{"nasi\n\tdua", "nasi dua", false},
		{"pedas!!!😀", "pedas", false},
		{"sambel+", "sambel+", false},
		{"123456789012345678901", "", true},
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

func TestMessage(t *testing.T) {
	Pick = func(int) int { return 0 }
	t.Cleanup(func() { Pick = rand.IntN })
	got := Message([]Line{
		{"DENIS", Daging, 2, "", ""}, {"DENIS", Daging, 2, "no tauge", ""}, {"SARI", Campur, 1, "", ""}, {"ARIS", Campur, 0, "pedes", ""},
	}, 0)
	want := "Mang! Mau pesen tangkar ya 🍢🥩😋\n\nDENIS: DAGING 2 SATE\nDENIS: DAGING 2 SATE (no tauge)\nSARI: CAMPUR 1 SATE\nARIS: CAMPUR (pedes)\n\nMakasih Mang 🙏"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	got = Message([]Line{{"BUDI", Campur, 2, "", ""}}, 1)
	want = "Mang, ada tambahan lagi nih 🙏🍢 (TAMBAHAN #1)\n\nBUDI: CAMPUR 2 SATE\n\nMakasih Mang 🙏"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(Message([]Line{{"A", Daging, 1, "", ""}}, 0), "Total") {
		t.Fatal("summary line came back")
	}
}

func TestNotice(t *testing.T) {
	got := Notice("Denis", []Line{{"", Daging, 1, "no tauge", ""}, {"", Campur, 0, "", ""}})
	want := "Halo Kak Denis 👋 orderan Kakak ke Mamang Tangkar sudah diproses ya:\n\n🍲 DAGING 1 SATE (no tauge)\n🍲 CAMPUR\n\nSelamat makan nanti 😋🍢"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestShortNames(t *testing.T) {
	got := ShortNames([]Person{
		{"denis.saputra@bfi.co.id", "Denis"},
		{"denis.wijaya@bfi.co.id", "Denis Wijaya"},
		{"sari@bfi.co.id", ""},
		{"m.rizki@bfi.co.id", "Muhammad Rizki"},
		{"budi1@bfi.co.id", "Budi"},
		{"budi2@bfi.co.id", "Budi"},
		{"x@bfi.co.id", "<b>"},
	})
	want := map[string]string{
		"denis.saputra@bfi.co.id": "DENIS S",
		"denis.wijaya@bfi.co.id":  "DENIS W",
		"sari@bfi.co.id":          "SARI",
		"m.rizki@bfi.co.id":       "MUHAMMAD",
		"budi1@bfi.co.id":         "BUDI1",
		"budi2@bfi.co.id":         "BUDI2",
		"x@bfi.co.id":             "B",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
}

func TestPhone(t *testing.T) {
	for in, want := range map[string]string{"0812-3456-789": "628123456789", "+62 812 3456 789": "628123456789", "8123456789": "628123456789"} {
		if got, err := Phone(in); err != nil || got != want {
			t.Errorf("Phone(%q)=%q %v", in, got, err)
		}
	}
	for _, bad := range []string{"021555", "12345", "", "6221123456"} {
		if _, err := Phone(bad); err == nil {
			t.Errorf("Phone(%q) accepted", bad)
		}
	}
}

func TestFriends(t *testing.T) {
	got := ShortNames([]Person{
		{"okky.pratama@bfi.co.id", "Okky"},
		{FriendKey("Okky"), "Okky"},
		{FriendKey("pak  Budi"), "pak  Budi"},
	})
	if got["okky.pratama@bfi.co.id"] != "OKKY" || got[FriendKey("okky")] != "OKKY" || got[FriendKey("Pak Budi")] != "PAK BUDI" {
		t.Fatalf("labels: %v", got)
	}
	if _, err := CleanFriend("Bapak Direktur Utama X"); err == nil {
		t.Fatal("long friend name accepted")
	}
	if n, _ := CleanFriend("<b>Okky</b>"); n != "bOkkyb" {
		t.Fatalf("friend not cleaned: %q", n)
	}
	msg := Notice("Aris", []Line{{Menu: Daging, Sate: 1, For: "Okky"}, {Menu: Campur}})
	if !strings.Contains(msg, "🍲 DAGING 1 SATE untuk Okky\n🍲 CAMPUR\n") {
		t.Fatalf("notice: %s", msg)
	}
}
