package main

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mohammed19n/passway/internal/server/store"
)

func TestACMEFlagsRequired(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // if validation passes, run returns at once instead of serving
	base := []string{"--mode", "self-hosted", "--domain", "example.com", "--tokens", "t", "--addr", "", "--tls-addr", "127.0.0.1:0", "--tls-mode", "acme"}

	t.Setenv("PASSWAY_CLOUDFLARE_API_TOKEN", "cf-secret-token")
	err := run(ctx, base, io.Discard, nil)
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "--acme-email") {
		t.Fatalf("no email: err = %v, exit %d; want usage error naming --acme-email, exit 2", err, exitCode(err))
	}

	t.Setenv("PASSWAY_CLOUDFLARE_API_TOKEN", "")
	err = run(ctx, append(base, "--acme-email", "ops@example.com"), io.Discard, nil)
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "PASSWAY_CLOUDFLARE_API_TOKEN") {
		t.Fatalf("no token: err = %v, exit %d; want usage error naming PASSWAY_CLOUDFLARE_API_TOKEN, exit 2", err, exitCode(err))
	}

	t.Setenv("PASSWAY_CLOUDFLARE_API_TOKEN", "cf-secret-token")
	for name, args := range map[string][]string{
		"bad mode":     {"--mode", "self-hosted", "--domain", "example.com", "--tokens", "t", "--tls-addr", "127.0.0.1:0", "--tls-mode", "magic"},
		"no tls-addr":  {"--mode", "self-hosted", "--domain", "example.com", "--tokens", "t", "--tls-mode", "acme", "--acme-email", "ops@example.com"},
		"cert in acme": append(append([]string{}, base...), "--acme-email", "ops@example.com", "--tls-cert", "c.pem", "--tls-key", "k.pem"),
	} {
		var out strings.Builder
		err := run(ctx, args, &out, nil)
		if exitCode(err) != 2 {
			t.Errorf("%s: err = %v, exit %d; want exit 2", name, err, exitCode(err))
		}
		if strings.Contains(out.String(), "cf-secret-token") || (err != nil && strings.Contains(err.Error(), "cf-secret-token")) {
			t.Errorf("%s: the Cloudflare token leaked", name)
		}
	}
}

func TestACMEDirDefault(t *testing.T) {
	if got, want := acmeDir("", "/var/lib/passway/p.db"), filepath.Join("/var/lib/passway", "certs"); got != want {
		t.Errorf("acmeDir(db) = %q, want %q", got, want)
	}
	if got := acmeDir("", ""); got != filepath.Join(".", "certs") {
		t.Errorf("acmeDir() = %q, want ./certs", got)
	}
	if got := acmeDir("/srv/certs", "/var/lib/passway/p.db"); got != "/srv/certs" {
		t.Errorf("acmeDir(flag) = %q, want the flag", got)
	}
}

// testACME points at a closed local port: nothing ever reaches a real CA or Cloudflare.
func testACME(t *testing.T) acmeOpts {
	return acmeOpts{email: "ops@example.com", dir: t.TempDir(), ca: "https://127.0.0.1:1/directory", cloudflareToken: "cf-secret-token"}
}

func TestNewCertManagerDecision(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, _, err := st.UpsertGitHubUser(ctx, store.User{GitHubID: 1, Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPlan(ctx, u.ID, "pro"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDomain(ctx, u.ID, "hooks.example.org", 2); err != nil {
		t.Fatal(err)
	}
	if err := st.VerifyDomain(ctx, u.ID, "hooks.example.org"); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cm, err := buildCertManager("passway.test", testACME(t), st, log)
	if err != nil {
		t.Fatal(err)
	}
	defer cm.cache.Stop()
	if cm.onDemand == nil || cm.onDemand.OnDemand == nil || cm.onDemand.OnDemand.DecisionFunc == nil {
		t.Fatal("cloud mode with a store must enable on-demand issuance")
	}
	decide := cm.onDemand.OnDemand.DecisionFunc
	for _, name := range []string{"random.example.net", "deep.sub.passway.test", "passway.test", ""} {
		if err := decide(ctx, name); err == nil {
			t.Errorf("decision(%q) = nil, want refusal", name)
		}
	}
	if err := decide(ctx, "hooks.example.org"); err != nil {
		t.Errorf("decision(verified pro domain) = %v, want nil", err)
	}
	if cm.wildcard.OnDemand != nil {
		t.Error("the wildcard config must never issue on demand")
	}

	// Through the real handshake path: a random SNI is refused, not issued.
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var logs lockedBuf
	dlog := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defer func() {
		if strings.Contains(logs.String(), "cf-secret-token") {
			t.Error("the Cloudflare token was logged")
		}
	}()
	tc, h, err := newCertManager(mctx, "passway.test", testACME(t), st, dlog)
	if err != nil {
		t.Fatal(err)
	}
	if tc.MinVersion < tls.VersionTLS12 || tc.GetCertificate == nil {
		t.Fatalf("tls config = %+v", tc)
	}
	if !contains(tc.NextProtos, "acme-tls/1") || !contains(tc.NextProtos, "http/1.1") {
		t.Errorf("NextProtos = %v, want http/1.1 and acme-tls/1", tc.NextProtos)
	}
	for _, sni := range []string{"random.example.net", "deep.sub.passway.test"} {
		if c, err := tc.GetCertificate(hello(t, sni)); err == nil || c != nil {
			t.Errorf("GetCertificate(%q) = %v, %v; want refusal", sni, c, err)
		}
	}
	if h == nil {
		t.Fatal("nil HTTP-01 handler")
	}
}

func TestNewCertManagerSelfHosted(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cm, err := buildCertManager("passway.test", testACME(t), nil, log)
	if err != nil {
		t.Fatal(err)
	}
	defer cm.cache.Stop()
	if cm.onDemand != nil {
		t.Fatal("without a store there must be no on-demand config")
	}
	if c, err := cm.getCertificate(hello(t, "random.example.net")); err == nil || c != nil {
		t.Errorf("random SNI: %v, %v; want refusal", c, err)
	}
	if _, err := buildCertManager("passway.test", acmeOpts{email: "a@b.c", dir: t.TempDir()}, nil, log); err == nil {
		t.Error("missing token must fail")
	}
}

func TestACMEHTTPHandler(t *testing.T) {
	challenge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := acmeHTTPHandler(challenge, redirectHandler(0))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://hooks.example.org/.well-known/acme-challenge/tok", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("challenge path: code %d, want the challenge handler", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://hooks.example.org/x?y=1", nil))
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://hooks.example.org/x?y=1" {
		t.Errorf("other path: code %d to %q, want the HTTPS redirect", rec.Code, rec.Header().Get("Location"))
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// hello is a ClientHello as a real handshake delivers it (certmagic logs Conn).
func hello(t *testing.T, sni string) *tls.ClientHelloInfo {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return &tls.ClientHelloInfo{ServerName: sni, Conn: a}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestRunACMERefusesRandomSNI(t *testing.T) {
	t.Setenv("PASSWAY_CLOUDFLARE_API_TOKEN", "cf-secret-token")
	addrs, stop := start(t, []string{"--mode", "self-hosted", "--domain", "passway.test", "--tokens", "t",
		"--addr", "127.0.0.1:0", "--tls-addr", "127.0.0.1:0", "--tls-mode", "acme",
		"--acme-email", "ops@example.com", "--acme-dir", t.TempDir(), "--acme-ca", "https://127.0.0.1:1/directory"})
	defer func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	}()
	conn, err := tls.Dial("tcp", addrs[1], &tls.Config{ServerName: "random.example.net", InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
		t.Fatal("handshake for a random SNI succeeded")
	}
	// The plain listener still redirects non-challenge requests.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get("http://" + addrs[0] + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("plain listener: %d, want 308", resp.StatusCode)
	}
}
