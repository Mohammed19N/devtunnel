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

// RateLimiter is a token bucket per key: capacity = perMinute, refill perMinute/60 per second.
type RateLimiter struct {
	mu        sync.Mutex
	perMinute int
	now       func() time.Time
	buckets   map[string]*bucket
	lastSweep time.Time
}

// NewRateLimiter returns a limiter allowing perMinute requests per key per minute.
func NewRateLimiter(perMinute int) *RateLimiter {
	return &RateLimiter{
		perMinute: perMinute,
		now:       time.Now,
		buckets:   make(map[string]*bucket),
		lastSweep: time.Now(),
	}
}

// Allow takes one token for key. When none is left it returns false and how long until one is.
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	capacity := float64(l.perMinute)
	rate := capacity / 60 // tokens per second

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
