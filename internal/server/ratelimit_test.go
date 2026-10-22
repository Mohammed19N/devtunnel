package server

import (
	"testing"
	"time"
)

func newTestLimiter(perMinute int) (*RateLimiter, *time.Time) {
	now := time.Unix(1_000_000, 0)
	l := NewRateLimiter(perMinute)
	l.now = func() time.Time { return now }
	l.lastSweep = now
	return l, &now
}

func TestRateLimiterBurstThenRefill(t *testing.T) {
	l, now := newTestLimiter(60)
	for i := 0; i < 60; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("allow %d denied", i)
		}
	}
	ok, retry := l.Allow("a")
	if ok {
		t.Fatal("61st allowed")
	}
	if retry < 900*time.Millisecond || retry > time.Second {
		t.Fatalf("retryAfter = %v, want in [900ms, 1s]", retry)
	}
	*now = now.Add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("not allowed after refill")
	}
}

func TestRateLimiterKeysIndependent(t *testing.T) {
	l, _ := newTestLimiter(3)
	for i := 0; i < 3; i++ {
		l.Allow("a")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a should be drained")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b should be unaffected")
	}
}

func TestRateLimiterSweep(t *testing.T) {
	l, now := newTestLimiter(60)
	l.Allow("a")
	*now = now.Add(11 * time.Minute)
	l.Allow("b")
	if n := len(l.buckets); n != 1 {
		t.Fatalf("len(buckets) = %d, want 1", n)
	}
}
