package server

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arisros/tangkar/internal/store"
)

const (
	asCookie   = "tangkar_as"
	asHeader   = "X-Tangkar-As"
	asTTL      = 30 * time.Minute
	personaDom = "@tangkar.test"
)

var personas = []store.Person{
	{Email: "uji1" + personaDom, Name: "Uji 1"},
	{Email: "uji2" + personaDom, Name: "Uji 2"},
	{Email: "uji3" + personaDom, Name: "Uji 3"},
}

func isPersona(email string) bool { return strings.HasSuffix(email, personaDom) }

// impersonating returns who the admin is acting as: from the TUI's header, or
// from a signed cookie bound to this admin that expires after asTTL.
func (s *Server) impersonating(r *http.Request, admin string) string {
	if r.Header.Get("Authorization") != "" {
		as := strings.ToLower(strings.TrimSpace(r.Header.Get(asHeader)))
		if as != "" && s.knownPerson(r.Context(), as) {
			return as
		}
		return ""
	}
	c, err := r.Cookie(asCookie)
	if err != nil {
		return ""
	}
	i := strings.LastIndex(c.Value, ".")
	if i < 0 {
		return ""
	}
	payload, sig := c.Value[:i], c.Value[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.sess.sign(payload))) {
		return ""
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 3 || parts[0] != admin {
		return ""
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return ""
	}
	return parts[1]
}

func (s *Server) knownPerson(ctx context.Context, email string) bool {
	for _, p := range personas {
		if p.Email == email {
			return true
		}
	}
	people, err := s.store.People(ctx)
	if err != nil {
		return false
	}
	for _, p := range people {
		if p.Email == email {
			return true
		}
	}
	return false
}

// realAdmin is an admin, whether or not they are impersonating right now.
func realAdmin(u User) string {
	if u.Real != "" {
		return u.Real
	}
	if u.Admin {
		return u.Email
	}
	return ""
}

func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request, u User) {
	if realAdmin(u) == "" {
		writeErr(w, http.StatusForbidden, "khusus admin")
		return
	}
	people, err := s.store.People(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := append([]store.Person{}, personas...)
	for _, p := range people {
		if !isPersona(p.Email) && !s.cfg.Admins[p.Email] {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": out})
}

func (s *Server) handleImpersonate(w http.ResponseWriter, r *http.Request, u User) {
	admin := realAdmin(u)
	if admin == "" {
		writeErr(w, http.StatusForbidden, "khusus admin")
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	as := strings.ToLower(strings.TrimSpace(in.Email))
	if as == "" || s.cfg.Admins[as] || !s.knownPerson(r.Context(), as) {
		writeErr(w, http.StatusBadRequest, "orang tidak dikenal")
		return
	}
	for _, p := range personas {
		if p.Email == as {
			if err := s.store.UpsertUser(r.Context(), p.Email, p.Name); err != nil {
				s.fail(w, err)
				return
			}
		}
	}
	if r.Header.Get("Authorization") == "" {
		payload := fmt.Sprintf("%s|%s|%d", admin, as, time.Now().Add(asTTL).Unix())
		http.SetCookie(w, &http.Cookie{
			Name: asCookie, Value: payload + "." + s.sess.sign(payload),
			Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookie, SameSite: http.SameSiteLaxMode, MaxAge: int(asTTL.Seconds()),
		})
	}
	s.log.Info("impersonate start", "admin", admin, "as", as)
	writeJSON(w, http.StatusOK, map[string]any{"email": as, "name": s.store.UserName(r.Context(), as), "expires_in": int(asTTL.Seconds())})
}

func (s *Server) handleImpersonateStop(w http.ResponseWriter, r *http.Request, u User) {
	http.SetCookie(w, &http.Cookie{Name: asCookie, Path: "/", MaxAge: -1})
	if u.Real != "" {
		s.log.Info("impersonate stop", "admin", u.Real, "as", u.Email)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

var errPersona = errors.New("UJI hanya boleh pesan di pesanan uji")

// guardWrite keeps test personas out of real orders and records every change
// an admin makes while impersonating.
func (s *Server) guardWrite(ctx context.Context, u User, orderID, what string) error {
	if u.Real == "" {
		return nil
	}
	if isPersona(u.Email) {
		o, err := s.store.Order(ctx, orderID)
		if err != nil {
			return err
		}
		if !o.Test {
			return errPersona
		}
	}
	s.log.Info("impersonated write", "admin", u.Real, "as", u.Email, "order", orderID, "action", what)
	return nil
}
