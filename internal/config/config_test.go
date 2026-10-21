package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.yaml")
	in := Config{Token: "tok", Server: "https://example.com"}
	if err := Save(p, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", fi.Mode().Perm())
	}
}

func TestConfigMissing(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || c != (Config{}) {
		t.Fatalf("got %+v, %v", c, err)
	}
}
