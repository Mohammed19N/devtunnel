package server

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Meter counts proxied bytes per account and enforces monthly bandwidth. Bytes
// are held in memory and flushed to the Directory periodically; Over and
// FreeCapHit read caches, never the database.
type Meter struct {
	d          Directory
	freeCap    int64
	flushEvery time.Duration
	log        *slog.Logger

	mu           sync.Mutex
	pending      map[string]*meterEntry // unflushed bytes by account ID
	usage        map[string]*meterUsage // cached DB usage by account ID
	freeTotal    int64                  // cached free-plan total from the DB
	freePending  int64                  // unflushed bytes of free-plan accounts
	inflight     map[string]int64       // bytes being written by a flush, not yet in the cached totals
	inflightFree int64
	warned       bool
}

type meterEntry struct {
	acc Account
	n   int64
}

type meterUsage struct {
	acc     Account
	n       int64
	touched bool          // used since the last flush; untouched entries are evicted
	ready   chan struct{} // non-nil (open) while the first load is in flight
}

// NewMeter creates a Meter. flushEvery <= 0 means 60s; freeCap <= 0 disables the global cap.
func NewMeter(d Directory, freeCap int64, flushEvery time.Duration, log *slog.Logger) *Meter {
	if flushEvery <= 0 {
		flushEvery = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Meter{d: d, freeCap: freeCap, flushEvery: flushEvery, log: log,
		pending: make(map[string]*meterEntry), usage: make(map[string]*meterUsage)}
}

// Add records n transferred bytes for acc.
func (m *Meter) Add(acc Account, n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.pending[acc.ID]
	if e == nil {
		e = &meterEntry{acc: acc}
		m.pending[acc.ID] = e
	}
	e.n += n
	if acc.Plan.Name == PlanFree.Name {
		m.freePending += n
	}
}

// Over reports whether acc has used up its plan's monthly bandwidth. Bandwidth 0 is never over.
func (m *Meter) Over(acc Account) bool {
	limit := acc.Plan.Bandwidth
	if limit <= 0 {
		return false
	}
	m.mu.Lock()
	u := m.usage[acc.ID]
	switch {
	case u == nil:
		// First sight of this account since the last flush: one caller loads,
		// concurrent ones wait. A failed load is cached as 0 (fail open) and
		// refreshed by the next flush, so errors never cause per-request I/O.
		u = &meterUsage{acc: acc, ready: make(chan struct{})}
		m.usage[acc.ID] = u
		ready := u.ready
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		n, err := m.d.Usage(ctx, acc)
		cancel()
		if err != nil {
			m.log.Warn("meter: load usage", "account", acc.ID, "err", err)
			n = 0
		}
		m.mu.Lock()
		u.n, u.ready = n, nil
		close(ready)
	case u.ready != nil:
		ready := u.ready
		m.mu.Unlock()
		<-ready
		m.mu.Lock()
	}
	u.touched = true
	total := u.n + m.inflight[acc.ID]
	if e := m.pending[acc.ID]; e != nil {
		total += e.n
	}
	m.mu.Unlock()
	return total >= limit
}

// FreeCapHit reports whether the Free plan's total this month has reached the global cap.
func (m *Meter) FreeCapHit() bool {
	if m.freeCap <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.freeTotal+m.freePending+m.inflightFree >= m.freeCap
}

// Run flushes every flushEvery and once more when ctx ends.
func (m *Meter) Run(ctx context.Context) {
	m.flush(ctx)
	t := time.NewTicker(m.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.flush(ctx)
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			m.flush(fctx)
			return
		}
	}
}

// flush writes unflushed bytes to the Directory and refreshes the caches.
// Bytes whose write fails stay pending for the next flush.
func (m *Meter) flush(ctx context.Context) {
	m.mu.Lock()
	batch := m.pending
	m.pending = make(map[string]*meterEntry)
	// The batch stays visible to Over/FreeCapHit until the refresh commits.
	m.inflight = make(map[string]int64, len(batch))
	m.inflightFree = m.freePending
	for id, e := range batch {
		m.inflight[id] = e.n
	}
	m.freePending = 0
	m.mu.Unlock()

	for id, e := range batch {
		if err := m.d.AddUsage(ctx, e.acc, e.n); err != nil {
			m.log.Warn("meter: flush usage", "account", id, "bytes", e.n, "err", err)
			m.mu.Lock()
			delete(m.inflight, id)
			if e.acc.Plan.Name == PlanFree.Name {
				m.inflightFree -= e.n
			}
			m.mu.Unlock()
			m.Add(e.acc, e.n)
			delete(batch, id)
		}
	}

	// Refresh the caches: accounts used since the last flush or just written.
	m.mu.Lock()
	refresh := make(map[string]Account)
	for id, u := range m.usage {
		if u.ready != nil {
			continue // first load still running
		}
		if !u.touched && batch[id] == nil {
			delete(m.usage, id)
			continue
		}
		u.touched = false
		refresh[id] = u.acc
	}
	for id, e := range batch {
		refresh[id] = e.acc
	}
	m.mu.Unlock()

	fresh := make(map[string]int64, len(refresh))
	for id, acc := range refresh {
		n, err := m.d.Usage(ctx, acc)
		if err != nil {
			m.log.Warn("meter: refresh usage", "account", id, "err", err)
			continue
		}
		fresh[id] = n
	}
	free, ferr := m.d.PlanUsage(ctx, PlanFree.Name)
	if ferr != nil {
		m.log.Warn("meter: refresh free total", "err", ferr)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflight, m.inflightFree = nil, 0
	for id := range refresh {
		// A failed refresh keeps the old cache plus what was just written.
		if _, ok := fresh[id]; !ok && batch[id] != nil {
			if u := m.usage[id]; u != nil {
				u.n += batch[id].n
			}
		}
	}
	for id, n := range fresh {
		if u := m.usage[id]; u != nil {
			u.n = n
		} else {
			m.usage[id] = &meterUsage{acc: refresh[id], n: n}
		}
	}
	if ferr != nil || m.freeCap <= 0 {
		return
	}
	m.freeTotal = free
	over80 := free+m.freePending >= m.freeCap/10*8
	if !over80 {
		m.warned = false // re-arm, e.g. after the month rolls over
	} else if !m.warned {
		m.warned = true
		m.log.Warn("free plan bandwidth at 80% of the monthly cap", "used", free+m.freePending, "cap", m.freeCap)
	}
}

// countingBody counts bytes read from a request body.
type countingBody struct {
	io.ReadCloser
	add func(int64)
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.add(int64(n))
	return n, err
}

// countingWriter counts response bytes and counts both directions of a
// hijacked (upgraded) connection.
type countingWriter struct {
	http.ResponseWriter
	add func(int64)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.add(int64(n))
	return n, err
}

func (w *countingWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *countingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &countingConn{Conn: conn, add: w.add}, brw, nil
}

type countingConn struct {
	net.Conn
	add func(int64)
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.add(int64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.add(int64(n))
	return n, err
}
