package toniebox

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// authToken is a syntactically valid Authorization header. The content handlers
// only require that the header is present and parses as digest mode, which means
// 3 characters of prefix plus 64 hex digits of token
// (src/handler_cloud.c:879-887, src/server.c:629-648); the value itself is never
// verified and httpServerAuthCallback returns HTTP_ACCESS_ALLOWED
// unconditionally (src/server.c:656-663).
var authToken = "BD " + strings.Repeat("f", 64)

// userAgent identifies the mock as a box. teddyCloud's security middleware only
// blocks crawler user agents and blacklisted host hashes
// (src/handler_security_mit.c:58-79, :124-154), so nothing here should trip it,
// but the requests look like a device's rather than curl's.
const UserAgent = "Toniebox/2.105.00 (Linux; mock)"

// ClientConfig describes how to reach teddyCloud's API listener.
type ClientConfig struct {
	// BaseURL is the listener root, e.g. https://teddycloud:443. Use an http
	// scheme for the plain listener; the box routes are registered SERTY_BOTH and
	// so are served on it (src/server.c:527).
	BaseURL string
	// Identity is the client certificate and its derived overlay.
	Identity *Identity
	// SNI selects teddyCloud's certificate chain. teddyCloud picks by whether SNI
	// is present at all: present means the TB2 chain, absent the TB1 chain
	// (src/server.c:718-725).
	SNI bool
	// Warn receives one-off diagnostics, for instance when plain HTTP is in use.
	Warn func(msg string, args ...any)
}

// Client is a box's connection to teddyCloud.
//
// It holds two connections, which teddyCloud treats identically because box
// identity comes from the client certificate rather than the connection: one raw
// TLS connection carrying the RTNL telemetry stream, and one ordinary HTTP
// client for the freshness and content routes. Per-overlay state — the freshness
// cache, the ear-press window, the firmware fields — is keyed on the overlay id
// derived from the CN, so both connections see the same box.
type Client struct {
	cfg  ClientConfig
	base string
	http *http.Client

	mu     sync.Mutex
	rtnl   net.Conn
	closed bool
}

// NewClient builds a client. It does not dial; RTNLConn does.
func NewClient(cfg ClientConfig) *Client {
	transport := &http.Transport{
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}
	if isTLS(cfg.BaseURL) {
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialTLS(ctx, cfg, network, addr)
		}
	}

	return &Client{
		cfg:  cfg,
		base: strings.TrimSuffix(cfg.BaseURL, "/"),
		http: &http.Client{Transport: transport},
	}
}

// isTLS reports whether base selects teddyCloud's HTTPS API listener. Only that
// listener can carry the RTNL stream, because the detection depends on a failed
// request-line parse of a peeked buffer
// (src/cyclone/cyclone_tcp/http/http_server.c:474-494).
func isTLS(base string) bool { return strings.HasPrefix(strings.ToLower(base), "https://") }

// baseHost returns the host and port of base, defaulting the port by scheme.
func baseHost(base string) string {
	stripped := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	stripped = strings.TrimSuffix(strings.Split(stripped, "/")[0], "/")
	if _, _, err := net.SplitHostPort(stripped); err != nil {
		// A URL without an explicit port is still a valid dial target; Go
		// normalises it here rather than failing later with an opaque error.
		scheme := "443"
		if !isTLS(base) {
			scheme = "80"
		}
		return net.JoinHostPort(stripped, scheme)
	}
	return stripped
}

func dialTLS(ctx context.Context, cfg ClientConfig, network, addr string) (net.Conn, error) {
	netDialer := &net.Dialer{Timeout: 10 * time.Second}
	if !isTLS(cfg.BaseURL) {
		return netDialer.DialContext(ctx, network, addr)
	}

	certs := []tls.Certificate(nil)
	if cfg.Identity != nil {
		certs = []tls.Certificate{cfg.Identity.Certificate}
	}
	dialer := &tls.Dialer{
		NetDialer: netDialer,
		Config: &tls.Config{
			// teddyCloud presents its own box certificate, which is either
			// self-signed or issued by a CA that is not in anyone's trust store.
			// It also never installs a trusted-CA list for client certificates
			// (src/server.c:794-795), so nothing here is verified in either
			// direction.
			InsecureSkipVerify: true,
			Certificates:       certs,
			ServerName:         sniHost(cfg),
		},
	}
	return dialer.DialContext(ctx, network, addr)
}

// sniHost is the value teddyCloud sees as SNI, and the presence of that value is
// the whole switch: SNI present selects the TB2 chain, absent the TB1 chain
// (src/server.c:718-725). It is deliberately independent of where the connection
// is dialled, since a box always speaks to the same host either way.
func sniHost(cfg ClientConfig) string {
	if !cfg.SNI {
		return ""
	}
	host, _, err := net.SplitHostPort(baseHost(cfg.BaseURL))
	if err != nil {
		return ""
	}
	return host
}

// Overlay returns the overlay id this client presents.
func (c *Client) Overlay() string { return c.cfg.Identity.Overlay }

// HTTPClient exposes the underlying client so the caller can reach teddyCloud
// routes this package does not model, notably the SSE feed.
func (c *Client) HTTPClient() *http.Client { return c.http }

// RTNLConn returns the long-lived telemetry connection, dialling on first use and
// redialling if the previous one dropped.
func (c *Client) RTNLConn(ctx context.Context) (net.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.rtnl != nil {
		return c.rtnl, nil
	}
	if c.closed {
		return nil, fmt.Errorf("toniebox: client is closed")
	}
	if !isTLS(c.cfg.BaseURL) {
		// The binary stream is recognised by both listeners — the detection is
		// in shared code (src/cyclone/cyclone_tcp/http/http_server.c:474-494) —
		// but the plain listener skips the certificate gate entirely
		// (src/server.c:314), so the CN never reaches get_settings_cn() and
		// every per-overlay key falls back to the default overlay. Useful for a
		// smoke test, wrong for anything that compares two boxes.
		if c.cfg.Warn != nil {
			c.cfg.Warn("mock-toniebox: plain HTTP selected; teddyCloud will key per-overlay state on the default overlay, not on the CN",
				"url", c.cfg.BaseURL)
		}
	}

	addr := baseHost(c.cfg.BaseURL)
	conn, err := dialTLS(ctx, c.cfg, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("toniebox: dialling RTNL on %s: %w", addr, err)
	}
	c.rtnl = conn
	return conn, nil
}

// WriteRTNL writes frames to the telemetry connection. A write failure drops the
// connection so the next call redials, which is what lets the caller treat a
// network fault as a fault rather than as a silent stall.
func (c *Client) WriteRTNL(frames ...[]byte) error {
	for _, frame := range frames {
		if err := c.writeRTNLFrame(frame); err != nil {
			c.dropRTNL()
			return err
		}
	}
	return nil
}

func (c *Client) writeRTNLFrame(frame []byte) error {
	c.mu.Lock()
	conn := c.rtnl
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("toniebox: RTNL connection is not established")
	}
	if _, err := conn.Write(frame); err != nil {
		return fmt.Errorf("toniebox: writing RTNL frame: %w", err)
	}
	return nil
}

func (c *Client) dropRTNL() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rtnl != nil {
		if err := c.rtnl.Close(); err != nil {
			slog.Debug("mock-toniebox: closing RTNL connection", "err", err)
		}
		c.rtnl = nil
	}
}

// DisconnectRTNL tears the telemetry connection down without closing the client,
// so the caller can observe how teddyCloud and the shim react to the box going
// away.
func (c *Client) DisconnectRTNL() { c.dropRTNL() }

// Close releases both connections.
func (c *Client) Close() error {
	c.dropRTNL()

	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	c.http.CloseIdleConnections()
	return nil
}

// Get issues an authenticated GET and returns the response. The caller closes
// the body.
func (c *Client) Get(ctx context.Context, path string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("toniebox: building GET %s: %w", path, err)
	}
	c.decorate(req, header)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("toniebox: GET %s: %w", path, err)
	}
	return resp, nil
}

// Post issues a POST with a fixed body and returns the response. The caller
// closes the body.
func (c *Client) Post(ctx context.Context, path, contentType string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("toniebox: building POST %s: %w", path, err)
	}
	c.decorate(req, nil)
	req.Header.Set("Content-Type", contentType)
	// teddyCloud reads the freshness body with a single httpReceive and no
	// WAIT_ALL (src/handler_cloud.c:1056), so it wants the body in one record.
	// Setting the length up front lets Go send it that way instead of chunking.
	req.ContentLength = int64(len(body))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("toniebox: POST %s: %w", path, err)
	}
	return resp, nil
}

func (c *Client) decorate(req *http.Request, header http.Header) {
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Authorization", authToken)
	for k, values := range header {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
}

// ReadLimited drains at most n bytes from r and returns them together with the
// number actually read. It is used for headers and small bodies where reading to
// EOF would hang on a response that is never going to end.
func ReadLimited(r io.Reader, n int64) ([]byte, error) {
	buf := make([]byte, n)
	read, err := io.ReadFull(r, buf)
	if err != nil && read == 0 && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:read], nil
}
