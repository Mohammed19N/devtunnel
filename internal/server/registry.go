package server

import (
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/mohammed19n/passway/internal/protocol"
)

// Tunnel is a registered tunnel owned by one client session.
type Tunnel struct {
	Name      string
	SessionID string

	session   *yamux.Session
	proxy     *httputil.ReverseProxy
	transport *http.Transport
}

// ClaimRequest describes the name a client wants.
type ClaimRequest struct{ Name, PreferredName, SessionID string }

type hold struct {
	sessionID  string
	releasedAt time.Time
}

// Registry tracks active tunnels and short post-disconnect name holds.
type Registry struct {
	mu     sync.Mutex
	hold   time.Duration
	now    func() time.Time
	active map[string]*Tunnel
	held   map[string]hold
}

// NewRegistry creates a registry whose released names stay reserved for holdFor.
func NewRegistry(holdFor time.Duration) *Registry {
	return &Registry{
		hold:   holdFor,
		now:    time.Now,
		active: make(map[string]*Tunnel),
		held:   make(map[string]hold),
	}
}

// heldBy returns the unexpired hold on name, purging it if expired. Caller holds mu.
func (r *Registry) heldBy(name string) (hold, bool) {
	h, ok := r.held[name]
	if !ok {
		return hold{}, false
	}
	if r.now().After(h.releasedAt.Add(r.hold)) {
		delete(r.held, name)
		return hold{}, false
	}
	return h, true
}

// available reports whether session may take name. An active tunnel owned by
// the same session does not block it (reconnect takeover). Caller holds mu.
func (r *Registry) available(name, session string) bool {
	if cur, ok := r.active[name]; ok {
		return cur.SessionID == session
	}
	if h, ok := r.heldBy(name); ok && h.sessionID != session {
		return false
	}
	return true
}

// grant registers t under name and returns the same-session tunnel it superseded, if any.
func (r *Registry) grant(name string, req ClaimRequest, t *Tunnel) (replaced *Tunnel) {
	replaced = r.active[name]
	delete(r.held, name)
	t.Name = name
	t.SessionID = req.SessionID
	r.active[name] = t
	return replaced
}

// Claim assigns a name to t per req and registers it. If the name is already
// active under the same SessionID (the client reconnected before the old
// connection was noticed dead), t takes it over and the superseded tunnel is
// returned so the caller can close it; its later Release is a no-op. A
// different SessionID gets name_taken for an active name.
func (r *Registry) Claim(req ClaimRequest, t *Tunnel) (replaced *Tunnel, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if req.Name != "" {
		if err := ValidateName(req.Name); err != nil {
			return nil, err
		}
		if !r.available(req.Name, req.SessionID) {
			return nil, &protocol.Error{Code: protocol.CodeNameTaken, Message: "name " + req.Name + " is taken"}
		}
		return r.grant(req.Name, req, t), nil
	}

	if req.PreferredName != "" && ValidateName(req.PreferredName) == nil &&
		r.available(req.PreferredName, req.SessionID) {
		return r.grant(req.PreferredName, req, t), nil
	}

	for i := 0; i < 100; i++ {
		name := RandomName()
		if _, ok := r.active[name]; ok {
			continue
		}
		if _, ok := r.heldBy(name); ok {
			continue
		}
		r.grant(name, req, t)
		return nil, nil
	}
	return nil, &protocol.Error{Code: protocol.CodeInternal, Message: "could not allocate a name"}
}

// Release removes t if it is the active holder of its name and starts the hold.
func (r *Registry) Release(t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[t.Name] != t {
		return
	}
	delete(r.active, t.Name)
	r.held[t.Name] = hold{sessionID: t.SessionID, releasedAt: r.now()}
}

// Lookup returns the active tunnel for name.
func (r *Registry) Lookup(name string) (*Tunnel, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.active[name]
	return t, ok
}

// All returns a snapshot of the active tunnels.
func (r *Registry) All() []*Tunnel {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Tunnel, 0, len(r.active))
	for _, t := range r.active {
		out = append(out, t)
	}
	return out
}
