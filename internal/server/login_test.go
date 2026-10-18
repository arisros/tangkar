package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

type attempt struct {
	state  string
	cookie *http.Cookie
}

func withOAuth(t *testing.T, e *env) {
	t.Helper()
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	t.Cleanup(token.Close)
	e.srv.oauth = &oauth2.Config{ClientID: "id", ClientSecret: "secret", RedirectURL: e.ts.URL + "/auth/callback",
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/auth", TokenURL: token.URL}}
}

func noFollow() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func login(t *testing.T, e *env, query string) attempt {
	t.Helper()
	resp, err := noFollow().Get(e.ts.URL + "/auth/login?" + query)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	return attempt{state: loc.Query().Get("state"), cookie: resp.Cookies()[0]}
}

func callback(t *testing.T, e *env, state string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+"/auth/callback?code=c&state="+url.QueryEscape(state), nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noFollow().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestLoginOverlappingAttempts(t *testing.T) {
	e := newEnv(t)
	withOAuth(t, e)
	first := login(t, e, "next=/o/abc")
	second := login(t, e, "next=/o/abc")
	if first.cookie.Name == second.cookie.Name {
		t.Fatal("attempts share one cookie")
	}
	resp := callback(t, e, first.state, first.cookie, second.cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("first attempt should pass the state check and reach the code exchange, got %d", resp.StatusCode)
	}
}

func TestLoginCookieLostRetriesOnce(t *testing.T) {
	e := newEnv(t)
	withOAuth(t, e)
	a := login(t, e, "next=/o/abc")
	resp := callback(t, e, a.state)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/auth/login?retry=1&next=%2Fo%2Fabc" {
		t.Fatalf("want a restart, got %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	b := login(t, e, "retry=1&next=/o/abc")
	resp = callback(t, e, b.state)
	body := new(strings.Builder)
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body.Write(buf[:n])
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body.String(), "Chrome atau Safari") || !strings.Contains(body.String(), "next=%2Fo%2Fabc") {
		t.Fatalf("second miss should explain, got %d %s", resp.StatusCode, body)
	}
}

func TestLoginTamperedState(t *testing.T) {
	e := newEnv(t)
	withOAuth(t, e)
	a := login(t, e, "next=/o/abc")
	parts := strings.Split(a.state, ".")
	parts[1] = "Ly9ldmlsLmNvbQ"
	resp := callback(t, e, strings.Join(parts, "."), a.cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Fatalf("tampered state accepted: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}
