package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// usageDir is a Directory whose usage is recorded in memory.
type usageDir struct {
	*fakeDirectory
	umu     sync.Mutex
	added   int64
	base    int64 // pre-existing usage for every account
	free    int64
	addErr  error
	loads   int
	banUser []string
	banHost []string
}

func (d *usageDir) AddUsage(_ context.Context, _ Account, n int64) error {
	d.umu.Lock()
	defer d.umu.Unlock()
	if d.addErr != nil {
		return d.addErr
	}
	d.added += n
	return nil
}
func (d *usageDir) Usage(context.Context, Account) (int64, error) {
	d.umu.Lock()
	defer d.umu.Unlock()
	d.loads++
	return d.base + d.added, nil
}
func (d *usageDir) PlanUsage(context.Context, string) (int64, error) {
	d.umu.Lock()
	defer d.umu.Unlock()
	return d.free + d.added, nil
}
func (d *usageDir) BannedLists(context.Context) ([]string, []string, error) {
	d.umu.Lock()
	defer d.umu.Unlock()
	return d.banUser, d.banHost, nil
}

func newUsageDir() *usageDir { return &usageDir{fakeDirectory: newFakeDirectory(PlanFree, "quick")} }

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMeterOverAndFlush(t *testing.T) {
	d := newUsageDir()
	m := NewMeter(d, 0, 0, quietLog())
	acc := Account{ID: "u1", Plan: PlanFree}
	m.Add(acc, 600<<20)
	if m.Over(acc) {
		t.Fatal("over at 600 MB")
	}
	m.Add(acc, 500<<20)
	if !m.Over(acc) {
		t.Fatal("not over at 1100 MB unflushed")
	}
	m.flush(context.Background())
	if d.added != 1100<<20 {
		t.Fatalf("AddUsage got %d, want %d", d.added, int64(1100<<20))
	}
	if !m.Over(acc) {
		t.Fatal("not over after flush")
	}
	if m.Over(Account{ID: "x", Plan: PlanUnlimited}) {
		t.Fatal("unlimited plan over")
	}
}

func TestMeterFlushFailureKeepsBytes(t *testing.T) {
	d := newUsageDir()
	d.addErr = errors.New("db down")
	m := NewMeter(d, 0, 0, quietLog())
	acc := Account{ID: "u1", Plan: PlanFree}
	m.Add(acc, 100)
	m.flush(context.Background())
	d.addErr = nil
	m.Add(acc, 50)
	m.flush(context.Background())
	if d.added != 150 {
		t.Fatalf("added = %d, want 150", d.added)
	}
}

func TestMeterOverDoesNotHitDBPerRequest(t *testing.T) {
	d := newUsageDir()
	m := NewMeter(d, 0, 0, quietLog())
	acc := Account{ID: "u1", Plan: PlanFree}
	for i := 0; i < 100; i++ {
		m.Over(acc)
	}
	if d.loads != 1 {
		t.Fatalf("Usage called %d times, want 1", d.loads)
	}
}

func TestMeterFreeCapWarnOnce(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, nil))
	d := newUsageDir()
	m := NewMeter(d, 1000, 0, log)
	acc := Account{ID: "u1", Plan: PlanFree}
	m.Add(acc, 700)
	m.flush(context.Background())
	if strings.Contains(buf.String(), "80%") || m.FreeCapHit() {
		t.Fatal("warned/hit below 80%")
	}
	m.Add(acc, 150)
	m.flush(context.Background())
	m.Add(acc, 10)
	m.flush(context.Background())
	if n := strings.Count(buf.String(), "80%"); n != 1 {
		t.Fatalf("warned %d times: %s", n, buf.String())
	}
	if m.FreeCapHit() {
		t.Fatal("cap hit at 860/1000")
	}
	m.Add(acc, 200)
	if !m.FreeCapHit() {
		t.Fatal("cap not hit at 1060/1000")
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
