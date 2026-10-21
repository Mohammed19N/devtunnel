package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunMissingDomain(t *testing.T) {
	err := run(context.Background(), nil, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--domain") {
		t.Fatalf("err = %v, want mention of --domain", err)
	}
}

func TestRunTLSWithoutCert(t *testing.T) {
	err := run(context.Background(), []string{"--domain", "localhost", "--tls-addr", ":0"}, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "--tls-cert") {
		t.Fatalf("err = %v, want mention of --tls-cert", err)
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
	addrs, stop := start(t, []string{"--domain", "localhost", "--addr", "127.0.0.1:0"})
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
		"--domain", "localhost", "--addr", "127.0.0.1:0",
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
