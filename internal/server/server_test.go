package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arisros/tangkar/internal/store"
	"github.com/arisros/tangkar/internal/wa"
	"github.com/arisros/tangkar/web"
)

const admin = "aris.kurniawan@bfi.co.id"

type env struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	st  *store.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewUnstartedServer(nil)
	base := "http://" + ts.Listener.Addr().String()
	srv, err := New(Config{
		BaseURL: base, SessionSecret: strings.Repeat("k", 32), AllowedDomain: "bfi.co.id",
		Admins: map[string]bool{admin: true, "arisjirat@gmail.com": true}, WADryRun: true,
	}, st, wa.New("", ""), web.FS, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	for _, u := range []string{admin, "budi@bfi.co.id"} {
		st.UpsertUser(context.Background(), u, strings.SplitN(u, "@", 2)[0])
	}
	return &env{t: t, srv: srv, ts: ts, st: st}
}

func (e *env) req(method, path, as, body string, origin bool) (*http.Response, map[string]any) {
	e.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if origin {
		req.Header.Set("Origin", e.ts.URL)
	}
	if as != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.srv.sess.issue(as, time.Now().Add(time.Hour))})
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (e *env) create() string {
	e.t.Helper()
	resp, out := e.req("POST", "/api/orders", admin, `{"title":"Soto <b>Tangkar</b>","lock":"23:59","late_minutes":0}`, true)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("create: %d %v", resp.StatusCode, out)
	}
	o := out["order"].(map[string]any)
	if o["title"] != "Soto bTangkarb" {
		e.t.Fatalf("title not cleaned: %q", o["title"])
	}
	return o["id"].(string)
}

func TestAuthAndOrigin(t *testing.T) {
	e := newEnv(t)
	if r, _ := e.req("GET", "/api/orders", "", "", false); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon api: %d", r.StatusCode)
	}
	r, _ := e.req("GET", "/o/abc", "", "", false)
	if r.StatusCode != http.StatusFound || !strings.HasPrefix(r.Header.Get("Location"), "/auth/login?next=%2Fo%2Fabc") {
		t.Fatalf("anon page: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("csp missing")
	}
	if r, _ := e.req("POST", "/api/orders", "budi@bfi.co.id", `{"lock":"23:59"}`, true); r.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin created an order: %d", r.StatusCode)
	}
	id := e.create()
	if r, _ := e.req("POST", "/api/orders/"+id+"/lines", "budi@bfi.co.id", `{"menu":"DAGING","sate":1,"note":""}`, false); r.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin write accepted: %d", r.StatusCode)
	}
	if r, _ := e.req("POST", "/api/orders/"+id+"/lock", "budi@bfi.co.id", `{}`, true); r.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin locked: %d", r.StatusCode)
	}
}

func TestLineValidation(t *testing.T) {
	e := newEnv(t)
	id := e.create()
	path := "/api/orders/" + id + "/lines"
	for _, bad := range []string{
		`{"menu":"SATE","sate":1,"note":""}`,
		`{"menu":"DAGING","sate":11,"note":""}`,
		`{"menu":"DAGING","sate":-1,"note":""}`,
		`{"menu":"DAGING","sate":1,"note":"12345678901"}`,
		`{"menu":"DAGING","sate":1,"note":"","qty":5}`,
		`not json`,
	} {
		if r, out := e.req("POST", path, "budi@bfi.co.id", bad, true); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s accepted: %d %v", bad, r.StatusCode, out)
		}
	}
	if r, _ := e.req("POST", path, "budi@bfi.co.id", `{"menu":"campur","sate":2,"note":"<img src=x onerror=alert(1)>"}`, true); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("long markup should exceed 10 after cleaning: %d", r.StatusCode)
	}
	if r, out := e.req("POST", path, "budi@bfi.co.id", `{"menu":"campur","sate":2,"note":"<b>pedes"}`, true); r.StatusCode != http.StatusCreated {
		t.Fatalf("valid line: %d %v", r.StatusCode, out)
	}
	_, snap := e.req("GET", "/api/orders/"+id, "budi@bfi.co.id", "", false)
	l := snap["lines"].([]any)[0].(map[string]any)
	if l["menu"] != "CAMPUR" || l["note"] != "bpedes" || l["name"] != "budi" {
		t.Fatalf("stored line: %v", l)
	}
}

func TestAllowedEmail(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		email, hd string
		ok        bool
	}{
		{"budi@bfi.co.id", "bfi.co.id", true},
		{"budi@bfi.co.id", "", false},
		{"budi@gmail.com", "", false},
		{"budi@evilbfi.co.id", "bfi.co.id", false},
		{"arisjirat@gmail.com", "", true},
	}
	for _, c := range cases {
		if got := e.srv.allowedEmail(c.email, c.hd); got != c.ok {
			t.Errorf("allowedEmail(%s,%s)=%v", c.email, c.hd, got)
		}
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/o/abc": "/o/abc", "//evil.com": "/", "https://evil.com": "/", "/\\evil.com": "/", "": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q)=%q", in, got)
		}
	}
}

func TestSSE(t *testing.T) {
	e := newEnv(t)
	id := e.create()
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/orders/"+id+"/events", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.srv.sess.issue("budi@bfi.co.id", time.Now().Add(time.Hour))})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %s", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	next := func() map[string]any {
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var m map[string]any
				json.Unmarshal([]byte(d), &m)
				return m
			}
		}
		t.Fatal("stream ended")
		return nil
	}
	first := next()
	if v := first["viewers"].([]any); len(v) != 1 || v[0] != "budi" {
		t.Fatalf("viewers %v", v)
	}
	e.req("POST", "/api/orders/"+id+"/lines", admin, `{"menu":"DAGING","sate":3,"note":""}`, true)
	for {
		s := next()
		if lines := s["lines"].([]any); len(lines) == 1 {
			break
		}
	}
}
