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
	"time"
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

	usageErr   error
	usageDelay time.Duration
	addStarted chan struct{} // when set, AddUsage signals here then waits on addGate
	addGate    chan struct{}
}

func (d *usageDir) AddUsage(_ context.Context, _ Account, n int64) error {
	if d.addStarted != nil {
		d.addStarted <- struct{}{}
		<-d.addGate
	}
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
	d.loads++
	delay, err := d.usageDelay, d.usageErr
	n := d.base + d.added
	d.umu.Unlock()
	time.Sleep(delay)
	if err != nil {
		return 0, err
	}
	return n, nil
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

func TestMeterUsageErrorCachedSingleFlight(t *testing.T) {
	d := newUsageDir()
	d.usageErr = errors.New("db down")
	d.usageDelay = 50 * time.Millisecond
	m := NewMeter(d, 0, 0, quietLog())
	acc := Account{ID: "u1", Plan: PlanFree}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Over(acc) }()
	}
	wg.Wait()
	m.Over(acc)
	d.umu.Lock()
	loads := d.loads
	d.umu.Unlock()
	if loads != 1 {
		t.Fatalf("Usage called %d times, want 1", loads)
	}
	d.umu.Lock()
	d.usageErr, d.usageDelay, d.base = nil, 0, PlanFree.Bandwidth
	d.umu.Unlock()
	if m.Over(acc) {
		t.Fatal("failed load should fail open until the next flush")
	}
	m.flush(context.Background())
	if !m.Over(acc) {
		t.Fatal("flush did not refresh the failed entry")
	}
}

func TestMeterFreeCapWarnRearms(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	d := newUsageDir()
	m := NewMeter(d, 1000, 0, slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, nil)))
	acc := Account{ID: "u1", Plan: PlanFree}
	m.Add(acc, 900)
	m.flush(context.Background())
	if n := strings.Count(buf.String(), "80%"); n != 1 {
		t.Fatalf("first crossing warned %d times", n)
	}
	d.umu.Lock()
	d.added = 0 // month rollover
	d.umu.Unlock()
	m.flush(context.Background())
	m.Add(acc, 900)
	m.flush(context.Background())
	if n := strings.Count(buf.String(), "80%"); n != 2 {
		t.Fatalf("after rollover warned %d times total, want 2", n)
	}
}

func TestMeterCountsInFlightBatch(t *testing.T) {
	d := newUsageDir()
	d.addStarted, d.addGate = make(chan struct{}), make(chan struct{})
	m := NewMeter(d, 1000, 0, quietLog())
	acc := Account{ID: "u1", Plan: PlanFree}
	m.Over(acc) // cache the account first
	m.Add(acc, PlanFree.Bandwidth)
	done := make(chan struct{})
	go func() { m.flush(context.Background()); close(done) }()
	<-d.addStarted
	if !m.Over(acc) {
		t.Error("Over undercounts while the batch is being written")
	}
	if !m.FreeCapHit() {
		t.Error("FreeCapHit undercounts while the batch is being written")
	}
	d.addGate <- struct{}{}
	<-done
	if !m.Over(acc) {
		t.Error("not over after flush")
	}
}

func TestMeterMonthRollover(t *testing.T) {
	d := newUsageDir()
	m := NewMeter(d, 0, 0, quietLog())
	acc := Account{ID: "u1", Plan: PlanFree}
	m.Add(acc, PlanFree.Bandwidth)
	m.flush(context.Background())
	if !m.Over(acc) {
		t.Fatal("not over")
	}
	d.umu.Lock()
	d.added = 0
	d.umu.Unlock()
	m.flush(context.Background())
	if m.Over(acc) {
		t.Fatal("still over after the month rolled over")
	}
}
