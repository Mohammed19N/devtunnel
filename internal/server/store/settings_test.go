package store

import (
	"bytes"
	"context"
	"testing"
)

func TestSettingStable(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	calls := 0
	a, err := s.Setting(ctx, "k", func() []byte { calls++; return []byte("secret") })
	if err != nil || string(a) != "secret" {
		t.Fatalf("a=%q err=%v", a, err)
	}
	b, err := s.Setting(ctx, "k", func() []byte { calls++; return []byte("other") })
	if err != nil || !bytes.Equal(a, b) || calls != 1 {
		t.Fatalf("a=%q b=%q calls=%d err=%v", a, b, calls, err)
	}
}

func TestSettingRejectsEmpty(t *testing.T) {
	s := openTest(t)
	if _, err := s.Setting(context.Background(), "k", func() []byte { return nil }); err == nil {
		t.Fatal("expected error for empty generator result")
	}
}
