package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	sessionCookie = "tangkar_session"
	stateCookie   = "tangkar_state"
	sessionTTL    = 30 * 24 * time.Hour
	userInfoURL   = "https://openidconnect.googleapis.com/v1/userinfo"
)

type User struct {
	Email string
	Name  string
	Admin bool
	// Real is the admin's own email while they impersonate Email.
	Real     string
	RealName string
}

type session struct{ key []byte }

func (s session) issue(email string, exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(email + "|" + strconv.FormatInt(exp.Unix(), 10)))
	return payload + "." + s.sign(payload)
}

func (s session) sign(payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s session) verify(v string, now time.Time) (string, error) {
	payload, sig, ok := strings.Cut(v, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return "", errors.New("bad session")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", err
	}
	email, expS, ok := strings.Cut(string(raw), "|")
	exp, err := strconv.ParseInt(expS, 10, 64)
	if !ok || err != nil || now.Unix() > exp {
		return "", errors.New("expired session")
	}
	return email, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     s.cfg.GoogleClientID,
		ClientSecret: s.cfg.GoogleClientSecret,
		RedirectURL:  s.cfg.BaseURL + "/auth/callback",
		Endpoint:     google.Endpoint,
		Scopes:       []string{"openid", "email", "profile"},
	}
}

// safeNext only accepts a local path, so the login round trip cannot be used
// as an open redirect.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}

// Each login attempt gets its own state cookie, so a second tap or a restored
// tab cannot overwrite the attempt Google is about to return to. The state
// itself is signed and carries the return path, which lets the callback start
// over once when the cookie stayed behind in another browser (an in-app
// browser handing off to Chrome or Safari).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		http.Error(w, "login belum dikonfigurasi", http.StatusServiceUnavailable)
		return
	}
	nonce := randomHex(16)
	retry := "0"
	if r.URL.Query().Get("retry") == "1" {
		retry = "1"
	}
	payload := nonce + "." + base64.RawURLEncoding.EncodeToString([]byte(safeNext(r.URL.Query().Get("next")))) + "." + retry
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie + "_" + nonce[:12], Value: nonce,
		Path: "/auth", HttpOnly: true, Secure: s.cfg.SecureCookie, SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	opts := []oauth2.AuthCodeOption{oauth2.AccessTypeOnline, oauth2.SetAuthURLParam("prompt", "select_account")}
	if s.cfg.AllowedDomain != "" {
		opts = append(opts, oauth2.SetAuthURLParam("hd", s.cfg.AllowedDomain))
	}
	http.Redirect(w, r, s.oauth.AuthCodeURL(payload+"."+s.sess.sign(payload), opts...), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		http.Error(w, "login belum dikonfigurasi", http.StatusServiceUnavailable)
		return
	}
	parts := strings.Split(r.URL.Query().Get("state"), ".")
	if len(parts) != 4 || len(parts[0]) != 32 || !hmac.Equal([]byte(parts[3]), []byte(s.sess.sign(strings.Join(parts[:3], ".")))) {
		s.loginTrouble(w, "/")
		return
	}
	nonce, retry := parts[0], parts[2]
	nextRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	next := safeNext(string(nextRaw))
	if r.URL.Query().Get("error") != "" {
		s.loginTrouble(w, next)
		return
	}
	cookie := stateCookie + "_" + nonce[:12]
	c, err := r.Cookie(cookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(nonce)) != 1 {
		if retry == "0" {
			s.log.Info("login cookie missing, starting over", "ua", r.UserAgent())
			http.Redirect(w, r, "/auth/login?retry=1&next="+url.QueryEscape(next), http.StatusFound)
			return
		}
		s.log.Warn("login cookie missing after retry", "ua", r.UserAgent())
		s.loginTrouble(w, next)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookie, Path: "/auth", MaxAge: -1})

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := s.oauth.Exchange(ctx, r.URL.Query().Get("code"))
	if err != nil {
		s.log.Warn("oauth exchange", "err", err)
		http.Error(w, "login Google gagal", http.StatusBadGateway)
		return
	}
	info, err := s.userInfo(ctx, tok)
	if err != nil {
		s.log.Warn("userinfo", "err", err)
		http.Error(w, "login Google gagal", http.StatusBadGateway)
		return
	}
	email := strings.ToLower(info.Email)
	if !info.EmailVerified || !s.allowedEmail(email, info.HD) {
		s.log.Warn("login rejected", "email", email, "hd", info.HD)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "Akun %s tidak diizinkan. Pakai email @%s.\n", email, s.cfg.AllowedDomain)
		return
	}
	name := info.GivenName
	if name == "" {
		name = info.Name
	}
	if name == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}
	if err := s.store.UpsertUser(ctx, email, clip(name, 40)); err != nil {
		s.fail(w, err)
		return
	}
	s.setSession(w, email)
	s.log.Info("login", "email", email)
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) setSession(w http.ResponseWriter, email string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.sess.issue(email, time.Now().Add(sessionTTL)),
		Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookie, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleDevLogin exists only for local runs (TANGKAR_DEV_LOGIN=1), never in the cluster.
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(r.URL.Query().Get("email"))
	name := r.URL.Query().Get("name")
	if email == "" || name == "" {
		http.Error(w, "email and name required", http.StatusBadRequest)
		return
	}
	if err := s.store.UpsertUser(r.Context(), email, clip(name, 40)); err != nil {
		s.fail(w, err)
		return
	}
	s.setSession(w, email)
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusFound)
}

func (s *Server) allowedEmail(email, hd string) bool {
	if s.cfg.Admins[email] {
		return true
	}
	d := s.cfg.AllowedDomain
	return d != "" && strings.HasSuffix(email, "@"+d) && strings.EqualFold(hd, d)
}

type userInfo struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	HD            string `json:"hd"`
}

func (s *Server) userInfo(ctx context.Context, tok *oauth2.Token) (userInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, userInfoURL, nil)
	resp, err := s.oauth.Client(ctx, tok).Do(req)
	if err != nil {
		return userInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return userInfo{}, fmt.Errorf("userinfo status %d", resp.StatusCode)
	}
	var u userInfo
	err = json.NewDecoder(resp.Body).Decode(&u)
	return u, err
}

// user resolves the caller from the session cookie, or from the personal
// bearer token the TUI got through device login.
func (s *Server) user(r *http.Request) (User, bool) {
	email := ""
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			return User{}, false
		}
		if email, ok = s.store.TokenEmail(r.Context(), tok); !ok {
			return User{}, false
		}
	} else {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			return User{}, false
		}
		if email, err = s.sess.verify(c.Value, time.Now()); err != nil {
			return User{}, false
		}
	}
	u := User{Email: email, Name: s.store.UserName(r.Context(), email), Admin: s.cfg.Admins[email]}
	if u.Admin {
		if as := s.impersonating(r, email); as != "" {
			return User{Email: as, Name: s.store.UserName(r.Context(), as), Real: email, RealName: u.Name}, true
		}
	}
	return u, true
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

const troublePage = `<!doctype html>
<html lang="id"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Tangkar</title><link rel="stylesheet" href="/static/style.css"></head>
<body><main>
<header class="top"><h1 class="brand"><a href="/">Tangkar</a></h1></header>
<p class="lede">Login belum berhasil, Kakak 🙏</p>
<p class="lede">Biasanya karena link dibuka dari browser di dalam aplikasi (WhatsApp, Telegram, Gmail). Buka link ini di Chrome atau Safari, lalu coba lagi.</p>
<a class="go" href="/auth/login?next=%s">Coba lagi</a>
</main></body></html>`

func (s *Server) loginTrouble(w http.ResponseWriter, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, troublePage, html.EscapeString(url.QueryEscape(next)))
}
