package wa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client sends through wa-gateway's POST /api/v1/send with a tenant token.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func New(url, token string) *Client {
	return &Client{URL: url, Token: token, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) Enabled() bool { return c.URL != "" && c.Token != "" }

func (c *Client) Send(ctx context.Context, to, message string) error {
	body, _ := json.Marshal(map[string]string{"to": to, "message": message})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api/v1/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("wa-gateway %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}
