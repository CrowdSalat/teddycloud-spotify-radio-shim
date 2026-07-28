// Package librespot provides a thin HTTP client for the go-librespot API.
package librespot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	defaultTimeout = 5 * time.Second

	// pauseTimeout is intentionally longer than defaultTimeout. The pause
	// command must wait for go-librespot's pipe driver to release out.lock,
	// which only happens once the FIFO consumer (pauseWithDrain) has drained
	// enough data to unblock the blocked file.Write. Under load this can take
	// several seconds, so 30 s gives the drain loop adequate headroom.
	pauseTimeout = 30 * time.Second
)

// Client calls the go-librespot REST API.
type Client struct {
	baseURL     string
	httpClient  *http.Client
	pauseClient *http.Client
}

// NewClient creates a Client targeting baseURL (e.g. "http://localhost:3678").
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:     baseURL,
		httpClient:  &http.Client{Timeout: defaultTimeout},
		pauseClient: &http.Client{Timeout: pauseTimeout},
	}
}

// Status is a subset of the go-librespot /status response.
type Status struct {
	Username string `json:"username"`
	Stopped  bool   `json:"stopped"`
	Paused   bool   `json:"paused"`
}

// Play tells go-librespot to start playing the given Spotify URI.
func (c *Client) Play(ctx context.Context, uri string) error {
	body, err := json.Marshal(map[string]string{"uri": uri})
	if err != nil {
		return err
	}
	return c.post(ctx, "/player/play", body)
}

// Pause pauses playback. Uses a dedicated HTTP client with a longer timeout
// (pauseTimeout) because the request must wait for go-librespot's pipe driver
// to release its internal mutex — see docs/research/go-librespot-pipe-deadlock.md.
func (c *Client) Pause(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/player/pause", nil)
	if err != nil {
		return err
	}

	resp, err := c.pauseClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d from /player/pause", resp.StatusCode)
	}

	return nil
}

// Resume resumes paused playback.
func (c *Client) Resume(ctx context.Context) error {
	return c.post(ctx, "/player/resume", nil)
}

// Next skips to the next track.
func (c *Client) Next(ctx context.Context) error {
	return c.post(ctx, "/player/next", nil)
}

// Prev skips to the previous track.
func (c *Client) Prev(ctx context.Context) error {
	return c.post(ctx, "/player/prev", nil)
}

// Status returns the current player status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/status", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d from /status", resp.StatusCode)
	}

	var s Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}

	return &s, nil
}

// post sends a POST request. body may be nil for endpoints with no payload.
func (c *Client) post(ctx context.Context, path string, body []byte) error {
	var req *http.Request
	var err error

	if body != nil {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost,
			c.baseURL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost,
			c.baseURL+path, nil)
	}
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d from %s", resp.StatusCode, path)
	}

	return nil
}
