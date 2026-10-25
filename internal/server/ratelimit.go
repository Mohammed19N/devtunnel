package server

import (
	"sync"
	"time"
)

const (
	sweepEvery = time.Minute
	sweepIdle  = 10 * time.Minute
)

type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimiter is a token bucket per key: capacity = n, refill n per period
// (a minute unless made with NewRateLimiterWindow).
type RateLimiter struct {
	mu        sync.Mutex
	perMinute int // the default n
	period    time.Duration
	now       func() time.Time
	buckets   map[string]*bucket
	lastSweep time.Time
}

// NewRateLimiter returns a limiter allowing perMinute requests per key per minute.
func NewRateLimiter(perMinute int) *RateLimiter {
	return NewRateLimiterWindow(perMinute, time.Minute)
}

// NewRateLimiterWindow returns a limiter allowing a burst of n requests per
// key, refilled at n per period, e.g. 5 per hour.
func NewRateLimiterWindow(n int, period time.Duration) *RateLimiter {
	return &RateLimiter{
		perMinute: n,
		period:    period,
		now:       time.Now,
		buckets:   make(map[string]*bucket),
		lastSweep: time.Now(),
	}
}

// Allow takes one token for key at the limiter's default rate. When none is
// left it returns false and how long until one is.
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	return l.AllowN(key, l.perMinute)
}

// AllowN is Allow with a per-key rate: perMinute is the capacity and refill
// for key's bucket, per minute (or per the limiter's period).
func (l *RateLimiter) AllowN(key string, perMinute int) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	capacity := float64(perMinute)
	rate := capacity / l.period.Seconds() // tokens per second

	if now.Sub(l.lastSweep) >= sweepEvery {
		l.lastSweep = now
		for k, b := range l.buckets {
			full := b.tokens+now.Sub(b.last).Seconds()*rate >= capacity
			if full && now.Sub(b.last) >= sweepIdle {
				delete(l.buckets, k)
			}
		}
	}

	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: capacity, last: now}
		l.buckets[key] = b
	} else {
		b.tokens = min(capacity, b.tokens+now.Sub(b.last).Seconds()*rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
}
