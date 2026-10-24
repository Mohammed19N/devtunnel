// Package inspector captures tunnel traffic and serves the local inspector UI.
package inspector

import (
	"net/http"
	"sync"
	"time"
)

// DefaultCapacity is the default number of exchanges kept in the ring.
const DefaultCapacity = 500

// MaxBody is the most bytes of any one body kept in a record.
const MaxBody = 1 << 20

// DefaultMaxTotalBytes caps the sum of request and response body bytes kept in
// a Store; the oldest exchanges are evicted first.
const DefaultMaxTotalBytes = 64 << 20

// Body is a captured message body.
type Body struct {
	Data      []byte `json:"data"` // at most MaxBody bytes (base64 in JSON)
	Size      int64  `json:"size"` // full size seen on the wire
	Truncated bool   `json:"truncated"`

	// Decoded is Data after gzip/deflate decoding (at most MaxBody bytes). It is
	// filled by the API for display only; the store keeps raw Data.
	Decoded          []byte `json:"decoded,omitempty"`
	DecodedTruncated bool   `json:"decoded_truncated,omitempty"`
}

// Request is a captured request.
type Request struct {
	Method string      `json:"method"`
	URI    string      `json:"uri"` // r.URL.RequestURI()
	Host   string      `json:"host"`
	Proto  string      `json:"proto"`
	Header http.Header `json:"header"`
	Body   Body        `json:"body"`
}

// Response is a captured response.
type Response struct {
	Status int         `json:"status"`
	Header http.Header `json:"header"`
	Body   Body        `json:"body"`
}

// Exchange is one request/response pair.
type Exchange struct {
	ID       int64         `json:"id"` // assigned by Store.Add, starting at 1
	Start    time.Time     `json:"start"`
	Duration time.Duration `json:"duration"` // ns
	Request  Request       `json:"request"`
	Response Response      `json:"response"`
	ReplayOf int64         `json:"replay_of,omitempty"` // 0 = live traffic
	Upgraded bool          `json:"upgraded,omitempty"`  // connection was hijacked (WebSocket)
}

// Summary is the list-view form of an Exchange.
type Summary struct {
	ID       int64         `json:"id"`
	Start    time.Time     `json:"start"`
	Duration time.Duration `json:"duration"`
	Method   string        `json:"method"`
	URI      string        `json:"uri"`
	Status   int           `json:"status"`
	ReplayOf int64         `json:"replay_of,omitempty"`
	Upgraded bool          `json:"upgraded,omitempty"`
}

// Summary returns the list-view form of e.
func (e *Exchange) Summary() Summary {
	return Summary{
		ID: e.ID, Start: e.Start, Duration: e.Duration,
		Method: e.Request.Method, URI: e.Request.URI,
		Status: e.Response.Status, ReplayOf: e.ReplayOf, Upgraded: e.Upgraded,
	}
}

// Event is a store change sent to subscribers: a new exchange, or Clear.
type Event struct {
	Clear   bool    // Clear was called; Summary is unset
	Summary Summary // the exchange just added
}

// Store is a bounded in-memory ring of exchanges with change subscribers.
// Exchanges must not be mutated after Add.
type Store struct {
	mu   sync.Mutex
	cap  int
	max  int64       // body-byte budget
	used int64       // body bytes held
	ring []*Exchange // oldest first
	next int64
	subs map[chan Event]struct{}
}

// NewStore returns a Store holding at most capacity exchanges (<= 0 means
// DefaultCapacity) and at most maxTotal body bytes (<= 0 means DefaultMaxTotalBytes).
// A single exchange over the byte budget is still kept, alone.
func NewStore(capacity int, maxTotal int64) *Store {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if maxTotal <= 0 {
		maxTotal = DefaultMaxTotalBytes
	}
	return &Store{cap: capacity, max: maxTotal, subs: map[chan Event]struct{}{}}
}

// Add assigns e.ID, stores e (evicting the oldest if full) and notifies subscribers
// without blocking.
func (s *Store) Add(e *Exchange) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	e.ID = s.next
	sz := bodyBytes(e)
	for len(s.ring) > 0 && (len(s.ring) >= s.cap || s.used+sz > s.max) {
		s.evictOldest()
	}
	s.ring = append(s.ring, e)
	s.used += sz
	s.notify(Event{Summary: e.Summary()})
	return e.ID
}

func (s *Store) evictOldest() {
	s.used -= bodyBytes(s.ring[0])
	copy(s.ring, s.ring[1:])
	s.ring[len(s.ring)-1] = nil
	s.ring = s.ring[:len(s.ring)-1]
}

func bodyBytes(e *Exchange) int64 {
	return int64(len(e.Request.Body.Data) + len(e.Response.Body.Data))
}

// Capacity returns the maximum number of exchanges held.
func (s *Store) Capacity() int { return s.cap }

// Get returns the exchange with the given id, if still held.
func (s *Store) Get(id int64) (*Exchange, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.ring {
		if e.ID == id {
			return e, true
		}
	}
	return nil, false
}

// List returns summaries, newest first.
func (s *Store) List() []Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Summary, 0, len(s.ring))
	for i := len(s.ring) - 1; i >= 0; i-- {
		out = append(out, s.ring[i].Summary())
	}
	return out
}

// Clear drops all held exchanges and notifies subscribers. IDs keep increasing.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = nil
	s.used = 0
	s.notify(Event{Clear: true})
}

// notify sends ev to every subscriber without blocking. Callers hold s.mu.
func (s *Store) notify(ev Event) {
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe returns a channel of store events (buffered 64; slow subscribers
// drop events) and a cancel func that closes the channel.
func (s *Store) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, ch)
			close(ch)
			s.mu.Unlock()
		})
	}
}
