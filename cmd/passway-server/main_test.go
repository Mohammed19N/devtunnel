package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohammed19n/passway/internal/client"
	"github.com/mohammed19n/passway/internal/protocol"
	"github.com/mohammed19n/passway/internal/server/store"
)

func TestRunMissingDomain(t *testing.T) {
	err := run(context.Background(), []string{"--mode", "self-hosted"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--domain") {
		t.Fatalf("err = %v, want mention of --domain", err)
	}
}

func TestRunTLSWithoutCert(t *testing.T) {
	err := run(context.Background(), []string{"--mode", "self-hosted", "--domain", "localhost", "--tls-addr", ":0"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--tls-cert") {
		t.Fatalf("err = %v, want mention of --tls-cert", err)
	}
}

func TestRunRejectsNegativeLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // if validation passes, run returns at once instead of serving
	for _, name := range []string{"--rate-limit", "--max-conns-per-tunnel"} {
		err := run(ctx, []string{"--mode", "self-hosted", "--domain", "localhost", "--addr", "127.0.0.1:0", name, "-1"}, io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), name) || exitCode(err) != 2 {
			t.Errorf("%s -1: err = %v, exit %d; want usage error mentioning the flag, exit 2", name, err, exitCode(err))
		}
	}
	if err := run(ctx, []string{"--rate-limit", "x"}, io.Discard, nil); exitCode(err) != 2 {
		t.Errorf("bad flag value: err = %v, exit %d, want 2", err, exitCode(err))
	}
	if c := exitCode(errors.New("listen failed")); c != 1 {
		t.Errorf("runtime error exit = %d, want 1", c)
	}
	if c := exitCode(flag.ErrHelp); c != 0 {
		t.Errorf("help exit = %d, want 0", c)
	}
}

// start runs the server in the background and returns the bound addresses and a stop function.
func start(t *testing.T, args []string) ([]string, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan []string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, args, io.Discard, func(a []string) { addrs <- a })
	}()
	select {
	case a := <-addrs:
		return a, func() error {
			cancel()
			select {
			case err := <-done:
				return err
			case <-time.After(11 * time.Second):
				t.Fatal("run did not return within 11s")
				return nil
			}
		}
	case err := <-done:
		cancel()
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server never became ready")
	}
	return nil, nil
}

func TestRunServesHealthz(t *testing.T) {
	addrs, stop := start(t, []string{"--mode", "self-hosted", "--domain", "localhost", "--addr", "127.0.0.1:0"})
	req, _ := http.NewRequest("GET", "http://"+addrs[0]+"/healthz", nil)
	req.Host = "localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := stop(); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
}

func writeSelfSigned(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost", "*.localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestRunRedirectsToTLS(t *testing.T) {
	cert, key := writeSelfSigned(t, t.TempDir())
	addrs, stop := start(t, []string{
		"--mode", "self-hosted", "--domain", "localhost", "--addr", "127.0.0.1:0",
		"--tls-addr", "127.0.0.1:0", "--tls-cert", cert, "--tls-key", key,
	})
	if len(addrs) != 2 {
		t.Fatalf("addrs = %v, want 2", addrs)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get("http://" + addrs[0] + "/x?y=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 308 {
		t.Fatalf("status = %d, want 308", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "https://") || !strings.HasSuffix(loc, "/x?y=1") {
		t.Fatalf("Location = %q", loc)
	}
	if err := stop(); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
}

func TestRunRefusesOpenRelay(t *testing.T) {
	err := run(context.Background(), []string{"--mode", "self-hosted", "--domain", "example.test", "--addr", "127.0.0.1:0"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--open") {
		t.Fatalf("err = %v, want refusal mentioning --open", err)
	}
}

func TestRunOpenFlagAllowsOpenRelay(t *testing.T) {
	_, stop := start(t, []string{"--mode", "self-hosted", "--domain", "example.test", "--addr", "127.0.0.1:0", "--open"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	_, stop = start(t, []string{"--mode", "self-hosted", "--domain", "example.test", "--addr", "127.0.0.1:0", "--tokens", "secret"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicPort(t *testing.T) {
	for _, c := range []struct {
		scheme                 string
		listen, override, want int
	}{
		{"http", 8080, 0, 8080},
		{"http", 80, 0, 0},
		{"https", 443, 0, 0},
		{"https", 9443, 0, 9443},
		{"https", 9443, 443, 0},
		{"http", 8080, 9000, 9000},
	} {
		if got := publicPort(c.scheme, c.listen, c.override); got != c.want {
			t.Errorf("publicPort(%q,%d,%d) = %d, want %d", c.scheme, c.listen, c.override, got, c.want)
		}
	}
}

func TestCloudModeRequiresFlags(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	for _, args := range [][]string{
		{"--mode", "cloud", "--domain", "example.test"},
		{"--mode", "cloud", "--domain", "example.test", "--db", db},
		{"--mode", "cloud", "--domain", "example.test", "--db", db, "--github-client-id", "id"},
		{"--mode", "cloud", "--domain", "example.test", "--github-client-id", "id", "--github-client-secret", "s"},
	} {
		err := run(context.Background(), append(args, "--addr", "127.0.0.1:0"), io.Discard, nil)
		want := "cloud mode needs --db, --github-client-id and --github-client-secret (or use --mode self-hosted)"
		if err == nil || err.Error() != want || exitCode(err) != 2 {
			t.Errorf("%v: err = %v, exit %d; want usage error %q", args, err, exitCode(err), want)
		}
	}
}

// The dashboard is only routed on Host == --domain, so --base-url must point there.
func TestBaseURLMustBeDomain(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // if validation passes, run returns at once instead of serving
	for _, base := range []string{"http://dash.localhost:8080", "http://example.test"} {
		err := run(ctx, []string{"--mode", "cloud", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--dev-login", "--base-url", base}, io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "--base-url") || exitCode(err) != 2 {
			t.Errorf("--base-url %s: err = %v, exit %d; want usage error mentioning --base-url", base, err, exitCode(err))
		}
	}
	_, stop := start(t, []string{"--mode", "cloud", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--dev-login", "--base-url", "http://LocalHost:9999"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCloudSecretFromEnv(t *testing.T) {
	t.Setenv("PASSWAY_GITHUB_CLIENT_SECRET", "s")
	db := filepath.Join(t.TempDir(), "p.db")
	_, stop := start(t, []string{"--mode", "cloud", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--github-client-id", "id"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCloudTokensFlagRejected(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	err := run(context.Background(), []string{"--mode", "cloud", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--dev-login", "--tokens", "a"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--tokens") || exitCode(err) != 2 {
		t.Fatalf("err = %v, exit %d; want usage error mentioning --tokens", err, exitCode(err))
	}
}

func TestDevLoginLocalhostOnly(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	err := run(context.Background(), []string{"--mode", "cloud", "--domain", "example.test", "--addr", "127.0.0.1:0", "--db", db, "--dev-login"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "localhost") {
		t.Fatalf("err = %v, want dev login refused off localhost", err)
	}
	_, stop := start(t, []string{"--mode", "cloud", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--dev-login"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCloudHTTPNonLocalhostFails(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	err := run(context.Background(), []string{"--mode", "cloud", "--domain", "example.test", "--addr", "127.0.0.1:0", "--db", db, "--github-client-id", "i", "--github-client-secret", "s"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v, want https startup error", err)
	}
}

func TestMinGitHubAgeDefault48h(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	cf := cloudFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *cf.minGitHubAge != 48*time.Hour {
		t.Fatalf("default = %v, want 48h", *cf.minGitHubAge)
	}
}

func TestSelfHostedDBTokens(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	st0, err := store.Open(db) // admin needs an existing database
	if err != nil {
		t.Fatal(err)
	}
	st0.Close()
	var out bytes.Buffer
	if err := runAdmin(context.Background(), []string{"--db", db, "token", "create", "laptop"}, &out); err != nil {
		t.Fatal(err)
	}
	var tok string
	for _, f := range strings.Fields(out.String()) {
		if strings.HasPrefix(f, "pw_") {
			tok = f
		}
	}
	if tok == "" {
		t.Fatalf("no token in %q", out.String())
	}
	// a cloud user's token must not work in self-hosted mode
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	u, _, _ := st.UpsertGitHubUser(context.Background(), store.User{GitHubID: 1, Login: "bob"})
	userTok, _, err := st.CreateToken(context.Background(), u.ID, "cli")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	addrs, stop := start(t, []string{"--mode", "self-hosted", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db})
	connect := func(token string) error { return connectOnce(addrs[0], token) }
	if err := connect(tok); err != nil {
		t.Errorf("admin token: %v", err)
	}
	if err := connect(userTok); err == nil {
		t.Error("cloud user's token accepted in self-hosted mode")
	}
	if err := connect("nope"); err == nil {
		t.Error("unknown token accepted")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCookieSecretPersisted(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	read := func() []byte {
		_, stop := start(t, []string{"--mode", "self-hosted", "--open", "--domain", "example.test", "--addr", "127.0.0.1:0", "--db", db})
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		b, err := st.Setting(context.Background(), "cookie_secret", func() []byte { return []byte("fresh") })
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	a, b := read(), read()
	if len(a) != 32 || !bytes.Equal(a, b) {
		t.Fatalf("secrets %x vs %x, want equal 32 bytes", a, b)
	}
}

func TestSelfHostedStaticAndDBTokens(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	adminTok, _, err := st.CreateToken(context.Background(), 0, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	addrs, stop := start(t, []string{"--mode", "self-hosted", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--tokens", "x"})
	for _, tok := range []string{"x", adminTok} {
		if err := connectOnce(addrs[0], tok); err != nil {
			t.Errorf("token %.6q: %v", tok, err)
		}
	}
	if err := connectOnce(addrs[0], "nope"); err == nil {
		t.Error("unknown token accepted")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardConfigMinGitHubAge(t *testing.T) {
	for _, c := range []struct {
		args []string
		want time.Duration
	}{{nil, 48 * time.Hour}, {[]string{"--min-github-age", "1h"}, time.Hour}} {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		cf := cloudFlags(fs)
		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		if got := cf.dashboardConfig(nil, "http://localhost", "s").MinGitHubAge; got != c.want {
			t.Errorf("%v: MinGitHubAge = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestCloudFinalFlushBeforeStoreClose(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	ctx := context.Background()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	u, _, _ := st.UpsertGitHubUser(ctx, store.User{GitHubID: 1, Login: "bob"})
	tok, _, _ := st.CreateToken(ctx, u.ID, "cli")
	if err := st.ClaimSubdomain(ctx, u.ID, "acme", true, 1); err != nil {
		t.Fatal(err)
	}
	st.Close()

	addrs, stop := start(t, []string{"--mode", "cloud", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--dev-login"})
	port := addrs[0][strings.LastIndex(addrs[0], ":")+1:]
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer local.Close()
	target, _ := url.Parse(local.URL)
	cctx, ccancel := context.WithCancel(ctx)
	ready := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = client.Run(cctx, client.Config{Server: "http://localhost:" + port, Token: tok, Name: "acme", Target: target,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OnReady: func(protocol.Ready) { ready <- struct{}{} }})
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel never ready")
	}
	req, _ := http.NewRequest("GET", "http://"+addrs[0]+"/", nil)
	req.Host = "acme.localhost:" + port
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	ccancel()
	<-done
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	used, err := st.Usage(ctx, u.ID, time.Now().UTC().Format("2006-01"))
	if err != nil || used < 4096 {
		t.Fatalf("usage = %d, %v; want the final meter flush to have landed (>= 4096)", used, err)
	}
}

// connectOnce connects a tunnel with token and returns nil once it is ready.
func connectOnce(addr, token string) error {
	target, _ := url.Parse("http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{}, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- client.Run(ctx, client.Config{
			Server: "http://localhost:" + addr[strings.LastIndex(addr, ":")+1:], Token: token, Target: target,
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			OnReady: func(protocol.Ready) { ready <- struct{}{}; cancel() },
		})
	}()
	select {
	case <-ready:
		<-errc
		return nil
	case err := <-errc:
		return err
	}
}

func TestModeDefaultSelfHosted(t *testing.T) {
	t.Setenv("PASSWAY_MODE", "")
	addrs, stop := start(t, []string{"--domain", "localhost", "--addr", "127.0.0.1:0"})
	if len(addrs) == 0 {
		t.Fatal("no listener")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

const cloudNeeds = "cloud mode needs --db, --github-client-id and --github-client-secret (or use --mode self-hosted)"

func TestModeFromEnv(t *testing.T) {
	t.Setenv("PASSWAY_MODE", "cloud")
	err := run(context.Background(), []string{"--domain", "example.test", "--addr", "127.0.0.1:0"}, io.Discard, nil)
	if err == nil || err.Error() != cloudNeeds || exitCode(err) != 2 {
		t.Fatalf("err = %v, exit %d; want usage error %q", err, exitCode(err), cloudNeeds)
	}
}

func TestModeFlagBeatsEnv(t *testing.T) {
	t.Setenv("PASSWAY_MODE", "cloud")
	_, stop := start(t, []string{"--mode", "self-hosted", "--domain", "localhost", "--addr", "127.0.0.1:0"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestModeInvalid(t *testing.T) {
	t.Setenv("PASSWAY_MODE", "")
	err := run(context.Background(), []string{"--mode", "hybrid", "--domain", "localhost", "--addr", "127.0.0.1:0"}, io.Discard, nil)
	want := `--mode must be cloud or self-hosted, got "hybrid"`
	if err == nil || err.Error() != want || exitCode(err) != 2 {
		t.Fatalf("err = %v, exit %d; want usage error %q", err, exitCode(err), want)
	}
}

func TestBillingFlagsPartial(t *testing.T) {
	t.Setenv("PASSWAY_STRIPE_SECRET_KEY", "")
	t.Setenv("PASSWAY_STRIPE_WEBHOOK_SECRET", "")
	db := filepath.Join(t.TempDir(), "p.db")
	err := run(context.Background(), []string{"--mode", "cloud", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--dev-login", "--stripe-price-pro", "price_1"}, io.Discard, nil)
	if err == nil || exitCode(err) != 2 ||
		!strings.Contains(err.Error(), "PASSWAY_STRIPE_SECRET_KEY") || !strings.Contains(err.Error(), "PASSWAY_STRIPE_WEBHOOK_SECRET") {
		t.Fatalf("err = %v, exit %d; want usage error naming both env vars", err, exitCode(err))
	}
	// Only the key set: names the other two inputs.
	t.Setenv("PASSWAY_STRIPE_SECRET_KEY", "sk_test_x")
	err = run(context.Background(), []string{"--mode", "cloud", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--dev-login"}, io.Discard, nil)
	if err == nil || exitCode(err) != 2 || !strings.Contains(err.Error(), "--stripe-price-pro") || !strings.Contains(err.Error(), "PASSWAY_STRIPE_WEBHOOK_SECRET") {
		t.Fatalf("key only: err = %v, exit %d", err, exitCode(err))
	}
}

func TestServeHTTPWithProxy(t *testing.T) {
	cert, key := writeSelfSigned(t, t.TempDir())
	addrs, stop := start(t, []string{
		"--mode", "self-hosted", "--domain", "localhost", "--addr", "",
		"--tls-addr", "127.0.0.1:0", "--tls-cert", cert, "--tls-key", key, "--proxy-protocol",
	})
	raw, err := net.Dial("tcp", addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	hdr := append([]byte("\r\n\r\n\x00\r\nQUIT\n"), 0x21, 0x11, 0x00, 0x0c, 203, 0, 113, 7, 10, 0, 0, 1, 0x10, 0xe1, 0x01, 0xbb)
	if _, err := raw.Write(hdr); err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tc, "GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := stop(); err != nil {
		t.Fatalf("run returned %v, want nil", err)
	}
}

func TestServerVersion(t *testing.T) {
	var buf bytes.Buffer
	runVersion(&buf)
	if got := buf.String(); got != "passway-server dev\n" {
		t.Fatalf("got %q", got)
	}
}
