package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/arisros/tangkar/internal/order"
	"github.com/arisros/tangkar/internal/store"
)

type snapshot struct {
	Order   store.Order  `json:"order"`
	Now     int64        `json:"now"`
	Lines   []store.Line `json:"lines"`
	Sends   []store.Send `json:"sends"`
	Totals  order.Totals `json:"totals"`
	Viewers []string     `json:"viewers"`
	CutAt   int64        `json:"cut_at"`
}

type summary struct {
	Order   store.Order  `json:"order"`
	Totals  order.Totals `json:"totals"`
	Viewers int          `json:"viewers"`
	Sends   []store.Send `json:"sends"`
}

type list struct {
	Now    int64     `json:"now"`
	Orders []summary `json:"orders"`
}

type me struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Admin    bool   `json:"admin"`
	ActingAs bool   `json:"acting_as"`
	RealName string `json:"real_name"`
}

type client struct {
	base  string
	token string
	http  *http.Client

	mu sync.Mutex
	as string
}

// setAs makes every following request act as email; "" stops impersonating.
func (c *client) setAs(email string) {
	c.mu.Lock()
	c.as = email
	c.mu.Unlock()
}

func (c *client) header(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	c.mu.Lock()
	if c.as != "" {
		req.Header.Set("X-Tangkar-As", c.as)
	}
	c.mu.Unlock()
}

var errLogin = errors.New("login expired")

func tokenPath(base string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tangkar", u.Host+".token"), nil
}

func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.header(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errLogin
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("server %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *client) call(method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return c.do(ctx, method, path, in, out)
}

// stream follows one SSE endpoint until ctx ends, reconnecting with backoff.
// Each event's data goes to onEvent; onState reports the connection.
func (c *client) stream(ctx context.Context, path string, onEvent func(event string, data []byte), onState func(bool)) {
	sc := &http.Client{}
	wait := time.Second
	for ctx.Err() == nil {
		err := c.streamOnce(ctx, sc, path, onEvent, func() { onState(true); wait = time.Second })
		onState(false)
		if errors.Is(err, errLogin) || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 15*time.Second)
	}
}

func (c *client) streamOnce(ctx context.Context, sc *http.Client, path string, onEvent func(string, []byte), onOpen func()) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.header(req)
	resp, err := sc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errLogin
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("events %d", resp.StatusCode)
	}
	onOpen()
	r := bufio.NewReaderSize(resp.Body, 64<<10)
	var event string
	var data []byte
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if event != "" && data != nil {
				onEvent(event, data)
			}
			event, data = "", nil
		case strings.HasPrefix(line, "event: "):
			event = line[7:]
		case strings.HasPrefix(line, "data: "):
			data = append(data, line[6:]...)
		}
	}
}

// login runs the device flow on plain stdout before the TUI starts.
func login(c *client) error {
	var start struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		URL        string `json:"url"`
		Interval   int    `json:"interval"`
		ExpiresIn  int    `json:"expires_in"`
	}
	c.token = ""
	if err := c.call(http.MethodPost, "/api/tui/login", map[string]string{}, &start); err != nil {
		return fmt.Errorf("start login: %w", err)
	}
	fmt.Printf("\n  Login tangkar\n\n  1. Buka:  %s\n  2. Login Google (@bfi.co.id), cek kode:  %s\n  3. Tekan \"Izinkan terminal\"\n\n  menunggu…", start.URL, start.UserCode)
	openBrowser(start.URL)
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(max(start.Interval, 1)) * time.Second)
		var tok struct {
			Token string `json:"token"`
		}
		err := c.call(http.MethodPost, "/api/tui/token", map[string]string{"device_code": start.DeviceCode}, &tok)
		if err != nil {
			return err
		}
		if tok.Token == "" {
			continue
		}
		c.token = tok.Token
		p, err := tokenPath(c.base)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		fmt.Print(" berhasil\n")
		return os.WriteFile(p, []byte(tok.Token), 0o600)
	}
	return errors.New("login kedaluwarsa, jalankan lagi")
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

func Logout(base string) error {
	p, err := tokenPath(base)
	if err != nil {
		return err
	}
	if b, err := os.ReadFile(p); err == nil {
		c := &client{base: base, token: strings.TrimSpace(string(b)), http: &http.Client{Timeout: 10 * time.Second}}
		_ = c.call(http.MethodPost, "/api/logout", map[string]string{}, nil)
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("logged out")
	return nil
}
