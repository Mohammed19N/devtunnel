package client

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/mohammed19n/passway/internal/protocol"
)

const (
	defaultInitialBackoff = 500 * time.Millisecond
	maxBackoff            = 30 * time.Second
	handshakeTimeout      = 10 * time.Second
)

// RequestLog describes one proxied request.
type RequestLog struct {
	Method, Path string
	Status       int
	Duration     time.Duration
}

// Config configures Run.
type Config struct {
	Server        string // "http://localhost:8080" or "https://passway.dev"
	Token         string
	Name          string // explicit --name
	Target        *url.URL
	RewriteHost   bool // --host-header=rewrite
	Insecure      bool // skip TLS verification (dev/self-signed only)
	ClientVersion string
	Logger        *slog.Logger
	OnReady       func(protocol.Ready) // called after every successful (re)connect
	OnRequest     func(RequestLog)     // called once per proxied request
	// Handler, when non-nil, replaces the default reverse proxy to Target as
	// the handler for tunnel streams (OnRequest logging still wraps it).
	Handler http.Handler

	initialBackoff time.Duration // test override
}

// ErrNotPassway is returned (fatal) when the first-ever dial gets an HTTP
// response other than 101, i.e. the server is not a Passway server.
var ErrNotPassway = errors.New("client: server is not a Passway server")

// Run connects to the server and serves the tunnel, reconnecting on
// non-fatal failures. It blocks until ctx is cancelled (returning nil) or a
// fatal protocol error occurs (returning it).
func Run(ctx context.Context, cfg Config) error {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Target == nil {
		return errors.New("client: nil target")
	}
	wsURL, err := tunnelURL(cfg.Server)
	if err != nil {
		return err
	}
	var sid [16]byte
	if _, err := rand.Read(sid[:]); err != nil {
		return fmt.Errorf("client: session id: %w", err)
	}
	c := &runner{cfg: cfg, log: log, wsURL: wsURL, sessionID: hex.EncodeToString(sid[:])}

	initial := cfg.initialBackoff
	if initial <= 0 {
		initial = defaultInitialBackoff
	}
	backoff := initial
	for {
		connected, err := c.once(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && (protocol.IsFatal(err) || errors.Is(err, ErrNotPassway)) {
			return err
		}
		if connected {
			backoff = initial
		}
		if err != nil {
			log.Warn("tunnel connection failed", "err", err)
		} else {
			log.Info("tunnel closed")
		}
		wait := jitter(backoff)
		log.Info("reconnecting", "in", wait)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// jitter scales d by a random factor in [0.8, 1.2).
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*mrand.Float64()))
}

func tunnelURL(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("client: invalid server URL %q: %w", server, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("client: server URL %q must be http or https", server)
	}
	u.Path = protocol.TunnelPath
	u.RawQuery = ""
	return u.String(), nil
}

type runner struct {
	cfg       Config
	log       *slog.Logger
	wsURL     string
	sessionID string
	lastName  string

	connectedOnce bool // a Ready has been received at least once
}

// once makes one connection attempt and serves it until it ends. connected
// reports whether a Ready was received.
func (c *runner) once(ctx context.Context) (connected bool, err error) {
	// connCtx owns the websocket's lifetime; cancelling it tears everything down.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(connCtx, handshakeTimeout)
	opts := &websocket.DialOptions{}
	if c.cfg.Insecure {
		opts.HTTPClient = &http.Client{Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // explicit insecure opt-in
		}}
	}
	ws, resp, err := websocket.Dial(dialCtx, c.wsURL, opts)
	dialCancel()
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			// 5xx and 429 (e.g. a reverse proxy while the server restarts) stay retryable.
			if !c.connectedOnce && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return false, fmt.Errorf("%w (HTTP %d from %s)", ErrNotPassway, resp.StatusCode, c.cfg.Server)
			}
			return false, fmt.Errorf("dial: HTTP %d from %s: %w", resp.StatusCode, c.cfg.Server, err)
		}
		return false, fmt.Errorf("dial: %w", err)
	}
	ws.SetReadLimit(-1)
	conn := websocket.NetConn(connCtx, ws, websocket.MessageBinary)

	ycfg := yamux.DefaultConfig()
	ycfg.EnableKeepAlive = true
	ycfg.KeepAliveInterval = 15 * time.Second
	ycfg.LogOutput = io.Discard
	sess, err := yamux.Client(conn, ycfg)
	if err != nil {
		conn.Close()
		return false, fmt.Errorf("yamux: %w", err)
	}
	defer sess.Close()

	ctrl, err := sess.OpenStream()
	if err != nil {
		return false, fmt.Errorf("open control stream: %w", err)
	}
	hello := protocol.Hello{
		Version:       protocol.Version,
		Token:         c.cfg.Token,
		Name:          c.cfg.Name,
		PreferredName: c.lastName,
		SessionID:     c.sessionID,
		ClientVersion: c.cfg.ClientVersion,
	}
	if err := protocol.WriteFrame(ctrl, hello); err != nil {
		return false, err
	}
	var hresp protocol.HelloResponse
	_ = ctrl.SetReadDeadline(time.Now().Add(handshakeTimeout))
	if err := protocol.ReadFrame(ctrl, &hresp); err != nil {
		return false, fmt.Errorf("reading handshake response: %w", err)
	}
	_ = ctrl.SetReadDeadline(time.Time{})
	if hresp.Error != nil {
		return false, hresp.Error
	}
	if hresp.Ready == nil {
		return false, errors.New("handshake response missing ready")
	}
	c.lastName = hresp.Ready.Name
	c.connectedOnce = true
	c.log.Info("tunnel ready", "url", hresp.Ready.URL)
	if c.cfg.OnReady != nil {
		c.cfg.OnReady(*hresp.Ready)
	}

	hs := newHTTPServer(c.cfg, c.log)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = hs.Serve(&sessionListener{sess: sess, log: c.log})
	}()

	select {
	case <-ctx.Done():
	case <-sess.CloseChan():
	}
	_ = hs.Close()
	sess.Close()
	cancel()
	<-serveDone
	return true, nil
}
