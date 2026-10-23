package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"log/slog"
	"math/big"
	"net/http"
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
	err := run(context.Background(), []string{"--self-hosted"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--domain") {
		t.Fatalf("err = %v, want mention of --domain", err)
	}
}

func TestRunTLSWithoutCert(t *testing.T) {
	err := run(context.Background(), []string{"--self-hosted", "--domain", "localhost", "--tls-addr", ":0"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--tls-cert") {
		t.Fatalf("err = %v, want mention of --tls-cert", err)
	}
}

func TestRunRejectsNegativeLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // if validation passes, run returns at once instead of serving
	for _, name := range []string{"--rate-limit", "--max-conns-per-tunnel"} {
		err := run(ctx, []string{"--self-hosted", "--domain", "localhost", "--addr", "127.0.0.1:0", name, "-1"}, io.Discard, nil)
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
	addrs, stop := start(t, []string{"--self-hosted", "--domain", "localhost", "--addr", "127.0.0.1:0"})
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
		"--self-hosted", "--domain", "localhost", "--addr", "127.0.0.1:0",
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
	err := run(context.Background(), []string{"--self-hosted", "--domain", "example.test", "--addr", "127.0.0.1:0"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--open") {
		t.Fatalf("err = %v, want refusal mentioning --open", err)
	}
}

func TestRunOpenFlagAllowsOpenRelay(t *testing.T) {
	_, stop := start(t, []string{"--self-hosted", "--domain", "example.test", "--addr", "127.0.0.1:0", "--open"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	_, stop = start(t, []string{"--self-hosted", "--domain", "example.test", "--addr", "127.0.0.1:0", "--tokens", "secret"})
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
		{"--domain", "example.test"},
		{"--domain", "example.test", "--db", db},
		{"--domain", "example.test", "--db", db, "--github-client-id", "id"},
		{"--domain", "example.test", "--github-client-id", "id", "--github-client-secret", "s"},
	} {
		err := run(context.Background(), append(args, "--addr", "127.0.0.1:0"), io.Discard, nil)
		want := "cloud mode needs --db, --github-client-id and --github-client-secret (or run your own relay with --self-hosted)"
		if err == nil || err.Error() != want || exitCode(err) != 2 {
			t.Errorf("%v: err = %v, exit %d; want usage error %q", args, err, exitCode(err), want)
		}
	}
}

func TestCloudSecretFromEnv(t *testing.T) {
	t.Setenv("PASSWAY_GITHUB_CLIENT_SECRET", "s")
	db := filepath.Join(t.TempDir(), "p.db")
	_, stop := start(t, []string{"--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--github-client-id", "id"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCloudTokensFlagRejected(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	err := run(context.Background(), []string{"--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--dev-login", "--tokens", "a"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--tokens") || exitCode(err) != 2 {
		t.Fatalf("err = %v, exit %d; want usage error mentioning --tokens", err, exitCode(err))
	}
}

func TestDevLoginLocalhostOnly(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	err := run(context.Background(), []string{"--domain", "example.test", "--addr", "127.0.0.1:0", "--db", db, "--dev-login"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "localhost") {
		t.Fatalf("err = %v, want dev login refused off localhost", err)
	}
	_, stop := start(t, []string{"--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db, "--dev-login"})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestCloudHTTPNonLocalhostFails(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
	err := run(context.Background(), []string{"--domain", "example.test", "--addr", "127.0.0.1:0", "--db", db, "--github-client-id", "i", "--github-client-secret", "s"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v, want https startup error", err)
	}
}

func TestMinGitHubAgeDefault(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	cf := cloudFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *cf.minGitHubAge != 168*time.Hour {
		t.Fatalf("default = %v, want 168h", *cf.minGitHubAge)
	}
}

func TestSelfHostedDBTokens(t *testing.T) {
	db := filepath.Join(t.TempDir(), "p.db")
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

	addrs, stop := start(t, []string{"--self-hosted", "--domain", "localhost", "--addr", "127.0.0.1:0", "--db", db})
	target, _ := url.Parse("http://127.0.0.1:1")
	connect := func(token string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ready := make(chan struct{}, 1)
		errc := make(chan error, 1)
		go func() {
			errc <- client.Run(ctx, client.Config{
				Server: "http://localhost:" + addrs[0][strings.LastIndex(addrs[0], ":")+1:], Token: token, Target: target,
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
		_, stop := start(t, []string{"--self-hosted", "--open", "--domain", "example.test", "--addr", "127.0.0.1:0", "--db", db})
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
