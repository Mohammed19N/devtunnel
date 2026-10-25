package passway

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func fakeRelease(t *testing.T, corrupt bool) (url string, paths func() []string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	stub := []byte("#!/bin/sh\necho \"passway 9.9.9\"\n")
	tw.WriteHeader(&tar.Header{Name: "passway", Mode: 0o755, Size: int64(len(stub))})
	tw.Write(stub)
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	hexsum := hex.EncodeToString(sum[:])
	if corrupt {
		hexsum = strings.Repeat("0", 64)
	}

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			// sha256sum format; include a decoy line for another file.
			w.Write([]byte(strings.Repeat("a", 64) + "  other.tar.gz\n"))
			for _, os := range []string{"linux", "darwin"} {
				for _, arch := range []string{"amd64", "arm64"} {
					w.Write([]byte(hexsum + "  passway_" + os + "_" + arch + ".tar.gz\n"))
				}
			}
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func runInstall(t *testing.T, env ...string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX only")
	}
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallScriptInstalls(t *testing.T) {
	url, _ := fakeRelease(t, false)
	dir := t.TempDir()
	out, err := runInstall(t, "PASSWAY_DOWNLOAD_URL="+url, "PASSWAY_INSTALL_DIR="+dir)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	fi, err := os.Stat(filepath.Join(dir, "passway"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatalf("not executable: %v", fi.Mode())
	}
	if !strings.Contains(out, "installed to") || !strings.Contains(out, "next: passway login <token>") {
		t.Fatalf("output = %q", out)
	}
}

func TestInstallScriptBadChecksum(t *testing.T) {
	url, _ := fakeRelease(t, true)
	dir := t.TempDir()
	out, err := runInstall(t, "PASSWAY_DOWNLOAD_URL="+url, "PASSWAY_INSTALL_DIR="+dir)
	if err == nil {
		t.Fatalf("expected failure\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "passway")); err == nil {
		t.Fatal("binary installed despite bad checksum")
	}
}

func TestInstallScriptPinnedVersion(t *testing.T) {
	url, paths := fakeRelease(t, false)
	dir := t.TempDir()
	out, err := runInstall(t, "PASSWAY_DOWNLOAD_URL="+url, "PASSWAY_INSTALL_DIR="+dir, "PASSWAY_VERSION=1.2.3")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	for _, p := range paths() {
		if !strings.HasPrefix(p, "/download/v1.2.3/") {
			t.Fatalf("request %q not under /download/v1.2.3/", p)
		}
	}
	if len(paths()) == 0 {
		t.Fatal("no requests")
	}
}

func TestInstallScriptEmbedded(t *testing.T) {
	disk, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk, InstallScript) {
		t.Fatal("InstallScript differs from install.sh")
	}
}
