package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func (e *env) as(method, path, admin string, as *http.Cookie, body string) (*http.Response, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", e.ts.URL)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.srv.sess.issue(admin, time.Now().Add(time.Hour))})
	if as != nil {
		req.AddCookie(as)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (e *env) impersonate(email string) *http.Cookie {
	e.t.Helper()
	resp, out := e.as("POST", "/api/impersonate", admin, nil, `{"email":"`+email+`"}`)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("impersonate %s: %d %v", email, resp.StatusCode, out)
	}
	for _, c := range resp.Cookies() {
		if c.Name == asCookie {
			return c
		}
	}
	e.t.Fatal("no impersonation cookie")
	return nil
}

func TestImpersonatePersona(t *testing.T) {
	e := newEnv(t)
	real := e.create()
	_, out := e.as("POST", "/api/orders", admin, nil, `{"title":"Uji","lock":"23:59","test":true}`)
	test := out["order"].(map[string]any)["id"].(string)

	if r, _ := e.req("POST", "/api/impersonate", "budi@bfi.co.id", `{"email":"uji1@tangkar.test"}`, true); r.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin impersonated: %d", r.StatusCode)
	}
	c := e.impersonate("uji1@tangkar.test")
	_, me := e.as("GET", "/api/me", admin, c, "")
	if me["email"] != "uji1@tangkar.test" || me["admin"] != false || me["acting_as"] != true || me["real_name"] != "aris.kurniawan" {
		t.Fatalf("me while impersonating: %v", me)
	}
	if r, out := e.as("POST", "/api/orders/"+real+"/lines", admin, c, `{"menu":"DAGING","sate":1,"note":""}`); r.StatusCode != http.StatusConflict {
		t.Fatalf("persona ordered in a real order: %d %v", r.StatusCode, out)
	}
	if r, _ := e.as("POST", "/api/orders", admin, c, `{"lock":"23:59"}`); r.StatusCode != http.StatusForbidden {
		t.Fatal("admin powers leaked into the impersonated view")
	}
	if r, out := e.as("POST", "/api/orders/"+test+"/lines", admin, c, `{"menu":"DAGING","sate":1,"note":""}`); r.StatusCode != http.StatusCreated {
		t.Fatalf("persona in test order: %d %v", r.StatusCode, out)
	}
	_, snap := e.req("GET", "/api/orders/"+test, admin, "", false)
	l := snap["lines"].([]any)[0].(map[string]any)
	if l["email"] != "uji1@tangkar.test" || l["acted_by"] != "aris.kurniawan" {
		t.Fatalf("line: %v", l)
	}
	e.as("POST", "/api/impersonate/stop", admin, c, "{}")
	stopped := *c
	stopped.Value = ""
	_, me = e.as("GET", "/api/me", admin, &stopped, "")
	if me["email"] != admin {
		t.Fatalf("still impersonating after stop: %v", me)
	}
}

func TestImpersonateRealUser(t *testing.T) {
	e := newEnv(t)
	id := e.create()
	c := e.impersonate("budi@bfi.co.id")
	if r, out := e.as("POST", "/api/orders/"+id+"/lines", admin, c, `{"menu":"CAMPUR","sate":2,"note":""}`); r.StatusCode != http.StatusCreated {
		t.Fatalf("order as budi: %d %v", r.StatusCode, out)
	}
	_, snap := e.req("GET", "/api/orders/"+id, admin, "", false)
	l := snap["lines"].([]any)[0].(map[string]any)
	if l["email"] != "budi@bfi.co.id" || l["acted_by"] != "aris.kurniawan" {
		t.Fatalf("line: %v", l)
	}
	if r, _ := e.as("POST", "/api/impersonate", admin, nil, `{"email":"nobody@bfi.co.id"}`); r.StatusCode != http.StatusBadRequest {
		t.Fatal("impersonated someone who never signed in")
	}
	if r, _ := e.as("POST", "/api/impersonate", admin, nil, `{"email":"arisjirat@gmail.com"}`); r.StatusCode != http.StatusBadRequest {
		t.Fatal("impersonated another admin")
	}
	other := *c
	other.Value = strings.Replace(c.Value, "budi@bfi.co.id", "uji2@tangkar.test", 1)
	_, me := e.as("GET", "/api/me", admin, &other, "")
	if me["email"] != admin {
		t.Fatalf("tampered cookie honoured: %v", me)
	}
}
