package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/arisros/tangkar/internal/order"
	"github.com/arisros/tangkar/internal/store"
	"github.com/arisros/tangkar/internal/wa"
	"golang.org/x/oauth2"
)

type Config struct {
	BaseURL            string
	GoogleClientID     string
	GoogleClientSecret string
	SessionSecret      string
	AllowedDomain      string
	Admins             map[string]bool
	SecureCookie       bool
	DevLogin           bool
	WATo               string
	WATestTo           string
	WADryRun           bool
}

type Server struct {
	cfg    Config
	store  *store.Store
	wa     *wa.Client
	hub    *hub
	sess   session
	oauth  *oauth2.Config
	log    *slog.Logger
	static fs.FS
	asset  string
	pages  map[string][]byte
	origin string
	wake   chan struct{}
}

func New(cfg Config, st *store.Store, w *wa.Client, static fs.FS, log *slog.Logger) (*Server, error) {
	if len(cfg.SessionSecret) < 32 {
		return nil, errors.New("session secret must be at least 32 bytes")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad base url %q", cfg.BaseURL)
	}
	s := &Server{
		cfg: cfg, store: st, wa: w, hub: newHub(), sess: session{key: []byte(cfg.SessionSecret)},
		log: log, static: static, origin: u.Scheme + "://" + u.Host, wake: make(chan struct{}, 1),
	}
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		s.oauth = s.oauthConfig()
	}
	if err := s.versionAssets(); err != nil {
		return nil, err
	}
	return s, nil
}

// versionAssets names this build's scripts and styles after their content and
// rewrites the pages to point there. Cloudflare stretches the browser cache of
// .js and .css to hours whatever the origin says, so a fixed URL would let a
// phone run yesterday's script against today's page.
func (s *Server) versionAssets() error {
	h := sha256.New()
	s.pages = map[string][]byte{}
	err := fs.WalkDir(s.static, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(s.static, path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".html") {
			s.pages[path] = b
		}
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	if err != nil {
		return err
	}
	s.asset = "v-" + hex.EncodeToString(h.Sum(nil))[:10]
	for name, b := range s.pages {
		s.pages[name] = bytes.ReplaceAll(b, []byte(`"/static/`), []byte(`"/static/`+s.asset+`/`))
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", s.handleHealth)
	m.HandleFunc("GET /auth/login", s.handleLogin)
	m.HandleFunc("GET /auth/callback", s.handleCallback)
	m.HandleFunc("GET /auth/logout", s.handleLogout)
	if s.cfg.DevLogin {
		m.HandleFunc("GET /auth/dev", s.handleDevLogin)
	}
	m.Handle("GET /static/", http.StripPrefix("/static/", s.staticFiles()))
	m.Handle("GET /favicon.ico", http.RedirectHandler("/static/favicon.svg", http.StatusMovedPermanently))
	m.HandleFunc("GET /{$}", s.page("index.html"))
	m.HandleFunc("GET /o/{id}", s.page("order.html"))
	m.HandleFunc("GET /link", s.page("link.html"))

	m.HandleFunc("POST /api/tui/login", s.handleTUILogin)
	m.HandleFunc("POST /api/tui/token", s.handleTUIToken)

	m.HandleFunc("GET /api/me", s.api(s.handleMe))
	m.HandleFunc("POST /api/link", s.api(s.handleLinkApprove))
	m.HandleFunc("POST /api/logout", s.api(s.handleTokenLogout))
	m.HandleFunc("GET /api/orders", s.api(s.handleList))
	m.HandleFunc("GET /api/events", s.api(s.handleListEvents))
	m.HandleFunc("POST /api/orders", s.admin(s.handleCreate))
	m.HandleFunc("GET /api/orders/{id}", s.api(s.handleOrder))
	m.HandleFunc("GET /api/orders/{id}/events", s.api(s.handleOrderEvents))
	m.HandleFunc("POST /api/orders/{id}/lines", s.api(s.handleAddLine))
	m.HandleFunc("DELETE /api/orders/{id}/lines/{line}", s.api(s.handleDeleteLine))
	m.HandleFunc("PUT /api/orders/{id}/lines/{line}", s.api(s.handleUpdateLine))
	m.HandleFunc("POST /api/orders/{id}/lock", s.admin(s.handleLock))
	m.HandleFunc("POST /api/orders/{id}/late", s.admin(s.handleLate))
	m.HandleFunc("POST /api/orders/{id}/cancel", s.admin(s.handleCancel))
	m.HandleFunc("POST /api/orders/{id}/sends/{batch}/retry", s.admin(s.handleRetry))
	m.HandleFunc("POST /api/phone/code", s.api(s.handlePhoneCode))
	m.HandleFunc("POST /api/phone/verify", s.api(s.handlePhoneVerify))
	m.HandleFunc("DELETE /api/phone", s.api(s.handlePhoneDelete))
	m.HandleFunc("POST /api/phones", s.admin(s.handlePhoneImport))
	m.HandleFunc("GET /api/people", s.api(s.handlePeople))
	m.HandleFunc("POST /api/impersonate", s.api(s.handleImpersonate))
	m.HandleFunc("POST /api/impersonate/stop", s.api(s.handleImpersonateStop))
	return s.secure(m)
}

const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"font-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

// staticFiles serves /static/<version>/file for good, and the bare path for
// pages still open from before a deploy.
func (s *Server) staticFiles() http.Handler {
	fsrv := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, rest, versioned := strings.Cut(r.URL.Path, "/")
		if versioned && strings.HasPrefix(first, "v-") {
			r.URL.Path = rest
		}
		if (versioned && first == s.asset) || strings.HasPrefix(r.URL.Path, "fonts/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fsrv.ServeHTTP(w, r)
	})
}

func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.user(r); !ok {
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(s.pages[name])
	}
}

type handler func(w http.ResponseWriter, r *http.Request, u User)

// api authenticates and, for cookie-borne writes, demands a same-origin
// request so another site cannot submit on someone's behalf.
func (s *Server) api(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.user(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "belum login")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Authorization") == "" && r.Header.Get("Origin") != s.origin {
			writeErr(w, http.StatusForbidden, "origin ditolak")
			return
		}
		next(w, r, u)
	}
}

func (s *Server) admin(next handler) http.HandlerFunc {
	return s.api(func(w http.ResponseWriter, r *http.Request, u User) {
		if !u.Admin {
			writeErr(w, http.StatusForbidden, "khusus admin")
			return
		}
		next(w, r, u)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrClosed), errors.Is(err, store.ErrSent), errors.Is(err, store.ErrTooMany), errors.Is(err, store.ErrPast), errors.Is(err, errPersona):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		s.log.Error("request failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "server error")
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "json only")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "input tidak valid")
		return false
	}
	return true
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "db", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u User) {
	phone, _ := s.store.Phone(r.Context(), u.Email)
	writeJSON(w, http.StatusOK, map[string]any{"email": u.Email, "name": u.Name, "admin": u.Admin, "phone": maskPhone(phone),
		"acting_as": u.Real != "", "real_name": u.RealName})
}

func maskPhone(p string) string {
	if len(p) < 8 {
		return p
	}
	return "0" + p[2:5] + strings.Repeat("•", len(p)-8) + p[len(p)-3:]
}

type snapshot struct {
	Order   store.Order  `json:"order"`
	Now     int64        `json:"now"`
	Lines   []store.Line `json:"lines"`
	Sends   []store.Send `json:"sends"`
	Totals  order.Totals `json:"totals"`
	Late    order.Totals `json:"late"`
	Viewers []string     `json:"viewers"`
	CutAt   int64        `json:"cut_at"`
}

func (s *Server) snapshot(ctx context.Context, id string) (snapshot, error) {
	o, err := s.store.Order(ctx, id)
	if err != nil {
		return snapshot{}, err
	}
	lines, err := s.store.Lines(ctx, id)
	if err != nil {
		return snapshot{}, err
	}
	sends, err := s.store.Sends(ctx, id)
	if err != nil {
		return snapshot{}, err
	}
	var all, late []order.Line
	var firstWaiting, lastWaiting int64
	for _, l := range lines {
		all = append(all, l.Order())
		if l.Late {
			late = append(late, l.Order())
			if l.Batch == nil {
				if firstWaiting == 0 || l.At < firstWaiting {
					firstWaiting = l.At
				}
				lastWaiting = max(lastWaiting, l.At)
			}
		}
	}
	snap := snapshot{
		Order: o, Now: time.Now().UnixMilli(), Lines: lines, Sends: sends,
		Totals: order.Sum(all), Late: order.Sum(late), Viewers: s.hub.viewers("order:" + id),
	}
	if lastWaiting > 0 {
		snap.CutAt = store.CutAt(time.Unix(firstWaiting, 0), time.Unix(lastWaiting, 0), o.LateUntil).UnixMilli()
	}
	return snap, nil
}

type summary struct {
	Order   store.Order  `json:"order"`
	Totals  order.Totals `json:"totals"`
	Viewers int          `json:"viewers"`
	Sends   []store.Send `json:"sends"`
}

func (s *Server) list(ctx context.Context) (map[string]any, error) {
	orders, err := s.store.Running(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]summary, 0, len(orders))
	for _, o := range orders {
		snap, err := s.snapshot(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		for i := range snap.Sends {
			snap.Sends[i].Message = ""
		}
		out = append(out, summary{Order: snap.Order, Totals: snap.Totals, Viewers: len(snap.Viewers), Sends: snap.Sends})
	}
	return map[string]any{"now": time.Now().UnixMilli(), "orders": out}, nil
}

// changed pushes fresh state to everyone watching the order and the list.
func (s *Server) changed(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if snap, err := s.snapshot(ctx, id); err == nil {
		s.hub.publish("order:"+id, frame("snapshot", snap))
	} else {
		s.log.Error("snapshot", "order", id, "err", err)
	}
	if l, err := s.list(ctx); err == nil {
		s.hub.publish(listTopic, frame("list", l))
	}
}

func frame(event string, v any) []byte {
	b, _ := json.Marshal(v)
	return []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, u User) {
	l, err := s.list(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleOrder(w http.ResponseWriter, r *http.Request, u User) {
	snap, err := s.snapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, sb *sub, first []byte) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "no streaming")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("retry: 3000\n\n"))
	if first != nil {
		w.Write(first)
	}
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case f := <-sb.ch:
			if _, err := w.Write(f); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request, u User) {
	l, err := s.list(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	sb := s.hub.join(listTopic, "", "")
	defer s.hub.leave(listTopic, sb)
	s.stream(w, r, sb, frame("list", l))
}

func (s *Server) handleOrderEvents(w http.ResponseWriter, r *http.Request, u User) {
	id := r.PathValue("id")
	if _, err := s.store.Order(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	topic := "order:" + id
	sb := s.hub.join(topic, u.Email, u.Name)
	defer func() {
		s.hub.leave(topic, sb)
		go s.changed(id)
	}()
	go s.changed(id)
	s.stream(w, r, sb, nil)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		Title       string `json:"title"`
		Lock        string `json:"lock"`
		LateMinutes *int   `json:"late_minutes"`
		Test        bool   `json:"test"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	title := clip(order.CleanText(in.Title), 40)
	if title == "" {
		title = "Soto Tangkar"
	}
	if in.Lock == "" {
		in.Lock = order.DefaultLock
	}
	lock, err := order.ClockOn(time.Now(), in.Lock)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	late := order.DefaultLate
	if in.LateMinutes != nil {
		if *in.LateMinutes < 0 || *in.LateMinutes > 240 {
			writeErr(w, http.StatusBadRequest, "late join 0 sampai 240 menit")
			return
		}
		late = time.Duration(*in.LateMinutes) * time.Minute
	}
	o, err := s.store.CreateOrder(r.Context(), title, lock, late, u.Email, in.Test)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("order created", "order", o.ID, "by", u.Email, "lock", lock, "test", in.Test)
	s.changed(o.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"order": o, "url": s.cfg.BaseURL + "/o/" + o.ID})
}

// readLine decodes and validates one bowl from a request body.
func readLine(w http.ResponseWriter, r *http.Request) (order.Line, bool) {
	var in struct {
		Menu string `json:"menu"`
		Sate int    `json:"sate"`
		Note string `json:"note"`
		For  string `json:"for"`
	}
	if !readJSON(w, r, &in) {
		return order.Line{}, false
	}
	menu, err := order.ValidMenu(in.Menu)
	if err == nil {
		err = order.ValidSate(in.Sate)
	}
	var note, friend string
	if err == nil {
		note, err = order.CleanNote(in.Note)
	}
	if err == nil {
		friend, err = order.CleanFriend(in.For)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return order.Line{}, false
	}
	return order.Line{Menu: menu, Sate: in.Sate, Note: note, For: friend}, true
}

func (s *Server) handleAddLine(w http.ResponseWriter, r *http.Request, u User) {
	l, ok := readLine(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := s.guardWrite(r.Context(), u, id, "add"); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.AddLineAs(r.Context(), id, u.Email, u.Real, l); err != nil {
		s.fail(w, err)
		return
	}
	s.changed(id)
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (s *Server) handleUpdateLine(w http.ResponseWriter, r *http.Request, u User) {
	lineID, err := strconv.ParseInt(r.PathValue("line"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "baris tidak valid")
		return
	}
	l, ok := readLine(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := s.guardWrite(r.Context(), u, id, "update"); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.UpdateLineAs(r.Context(), id, lineID, u.Email, u.Real, l); err != nil {
		s.fail(w, err)
		return
	}
	s.changed(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteLine(w http.ResponseWriter, r *http.Request, u User) {
	lineID, err := strconv.ParseInt(r.PathValue("line"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "baris tidak valid")
		return
	}
	id := r.PathValue("id")
	if err := s.guardWrite(r.Context(), u, id, "delete"); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteLine(r.Context(), id, lineID, u.Email); err != nil {
		s.fail(w, err)
		return
	}
	s.changed(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLock(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		At string `json:"at"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	var err error
	if in.At == "" {
		err = s.store.LockNow(r.Context(), id)
	} else {
		var at time.Time
		if at, err = order.ClockOn(time.Now(), in.At); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		err = s.store.SetLockAt(r.Context(), id, at)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("lock changed", "order", id, "by", u.Email, "at", in.At)
	s.kick()
	s.changed(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLate sets the late-join cutoff to minutes after the lock; 0 closes it.
func (s *Server) handleLate(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		Minutes int `json:"minutes"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Minutes < 0 || in.Minutes > 240 {
		writeErr(w, http.StatusBadRequest, "late join 0 sampai 240 menit")
		return
	}
	id := r.PathValue("id")
	o, err := s.store.Order(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	at := o.LockAt.Add(time.Duration(in.Minutes) * time.Minute)
	if in.Minutes == 0 || at.Before(time.Now()) {
		at = time.Now()
	}
	if err := s.store.SetLateUntil(r.Context(), id, at); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("late join changed", "order", id, "by", u.Email, "minutes", in.Minutes)
	s.kick()
	s.changed(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request, u User) {
	id := r.PathValue("id")
	if err := s.store.Cancel(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("order cancelled", "order", id, "by", u.Email)
	s.changed(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request, u User) {
	id := r.PathValue("id")
	batch, err := strconv.Atoi(r.PathValue("batch"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "batch tidak valid")
		return
	}
	if err := s.store.RetrySend(r.Context(), id, batch); err != nil {
		s.fail(w, err)
		return
	}
	s.kick()
	s.changed(id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTUILogin(w http.ResponseWriter, r *http.Request) {
	device, code, err := s.store.StartLogin(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": device, "user_code": code, "interval": 2, "expires_in": 600,
		"url": s.cfg.BaseURL + "/link?code=" + url.QueryEscape(code),
	})
}

func (s *Server) handleTUIToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DeviceCode string `json:"device_code"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	tok, err := s.store.PollLogin(r.Context(), in.DeviceCode)
	switch {
	case errors.Is(err, store.ErrPending):
		writeErr(w, http.StatusAccepted, "pending")
	case err != nil:
		s.fail(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"token": tok})
	}
}

func (s *Server) handleLinkApprove(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if r.Header.Get("Authorization") != "" {
		writeErr(w, http.StatusForbidden, "setujui dari browser")
		return
	}
	if err := s.store.ApproveLogin(r.Context(), in.Code, u.Email); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "kode salah atau kedaluwarsa")
			return
		}
		s.fail(w, err)
		return
	}
	s.log.Info("tui login approved", "email", u.Email)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTokenLogout(w http.ResponseWriter, r *http.Request, u User) {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		_ = s.store.RevokeToken(r.Context(), tok)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePhoneCode(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		Phone string `json:"phone"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	msisdn, err := order.Phone(in.Phone)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	code, err := s.store.StartPhoneCode(r.Context(), u.Email, msisdn)
	if errors.Is(err, store.ErrWait) {
		writeErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	msg := "Kode tangkar Kakak: " + code + " (berlaku 10 menit) 🍢\nAbaikan kalau bukan Kakak yang minta ya 🙏"
	if err := s.sendWA(r.Context(), msisdn, msg); err != nil {
		s.log.Warn("phone code send failed", "email", u.Email, "err", err)
		writeErr(w, http.StatusBadGateway, "gagal kirim kode ke WhatsApp, coba lagi nanti")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePhoneVerify(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	msisdn, err := s.store.VerifyPhoneCode(r.Context(), u.Email, in.Code)
	if errors.Is(err, store.ErrCode) || errors.Is(err, store.ErrTries) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("phone verified", "email", u.Email)
	writeJSON(w, http.StatusOK, map[string]string{"phone": maskPhone(msisdn)})
}

func (s *Server) handlePhoneDelete(w http.ResponseWriter, r *http.Request, u User) {
	if err := s.store.DeletePhone(r.Context(), u.Email); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePhoneImport(w http.ResponseWriter, r *http.Request, u User) {
	var in struct {
		Text string `json:"text"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || dec.Decode(&in) != nil {
		writeErr(w, http.StatusBadRequest, "input tidak valid")
		return
	}
	n, bad, err := s.store.ImportPhones(r.Context(), in.Text)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("phones imported", "by", u.Email, "saved", n, "rejected", len(bad))
	writeJSON(w, http.StatusOK, map[string]any{"saved": n, "rejected": bad})
}
