package server

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/mohammed19n/passway/internal/protocol"
)

// Tunnel is a registered tunnel owned by one client session.
type Tunnel struct {
	Name          string
	SessionID     string
	Account       Account
	ClientVersion string
	ConnectedAt   time.Time

	requests  atomic.Int64
	active    atomic.Int32 // requests in flight through the edge
	claimedBy string       // ClaimRequest.AccountID; its holds are free to the same account
	session   *yamux.Session
	proxy     *httputil.ReverseProxy
	transport *http.Transport
}

// Requests returns the number of requests proxied through t.
func (t *Tunnel) Requests() int64 { return t.requests.Load() }

// ClaimRequest describes the name a client wants. With MaxTunnels > 0, Claim
// refuses the tunnel when AccountID already has that many active tunnels under
// other sessions. Host, when set, is a custom domain (already normalised and
// ownership-checked by the caller) claimed as the key instead of a label;
// a full hostname always has a dot, so it never collides with a label.
type ClaimRequest struct {
	Name, PreferredName, SessionID string
	Host                           string
	AccountID, PlanName            string
	MaxTunnels                     int
}

type hold struct {
	sessionID  string
	accountID  string // "" = held for sessionID only
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

// available reports whether req may take name. An active tunnel owned by the
// same session does not block it (reconnect takeover). A hold does not block
// its own session, nor a new session of the same account (a restarted cloud
// client). Caller holds mu.
func (r *Registry) available(name string, req ClaimRequest) bool {
	if cur, ok := r.active[name]; ok {
		return cur.SessionID == req.SessionID
	}
	if h, ok := r.heldBy(name); ok && h.sessionID != req.SessionID &&
		(req.AccountID == "" || h.accountID != req.AccountID) {
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
	t.claimedBy = req.AccountID
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

	key := req.Name
	if req.Host != "" {
		key = req.Host
	}
	if req.MaxTunnels > 0 {
		n := r.countAccount(req.AccountID, "")
		// Taking over the same session's active tunnel replaces it, so it is not extra.
		if cur, ok := r.active[key]; ok && key != "" && cur.SessionID == req.SessionID && cur.Account.ID == req.AccountID {
			n--
		}
		if n >= req.MaxTunnels {
			return nil, limitError(req.PlanName, req.MaxTunnels)
		}
	}

	if req.Host != "" {
		if req.Name != "" || !strings.Contains(req.Host, ".") {
			return nil, &protocol.Error{Code: protocol.CodeInvalidName, Message: "invalid custom domain"}
		}
		if !r.available(req.Host, req) {
			return nil, &protocol.Error{Code: protocol.CodeNameTaken, Message: req.Host + " is already in use by another session"}
		}
		return r.grant(req.Host, req, t), nil
	}

	if req.Name != "" {
		if err := ValidateName(req.Name); err != nil {
			return nil, err
		}
		if !r.available(req.Name, req) {
			return nil, &protocol.Error{Code: protocol.CodeNameTaken, Message: "name " + req.Name + " is taken"}
		}
		return r.grant(req.Name, req, t), nil
	}

	if req.PreferredName != "" && ValidateName(req.PreferredName) == nil &&
		r.available(req.PreferredName, req) {
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

// purgeExpired deletes every expired hold. Caller holds mu.
func (r *Registry) purgeExpired() {
	now := r.now()
	for name, h := range r.held {
		if now.After(h.releasedAt.Add(r.hold)) {
			delete(r.held, name)
		}
	}
}

// Release removes t if it is the active holder of its name and starts the hold.
func (r *Registry) Release(t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeExpired()
	if r.active[t.Name] != t {
		return
	}
	delete(r.active, t.Name)
	r.held[t.Name] = hold{sessionID: t.SessionID, accountID: t.claimedBy, releasedAt: r.now()}
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

// limitError is the error for a plan whose concurrent-tunnel limit is reached.
// The plan name is shown capitalised ("Free").
func limitError(plan string, max int) *protocol.Error {
	if plan != "" {
		plan = strings.ToUpper(plan[:1]) + plan[1:]
	}
	return &protocol.Error{Code: protocol.CodeLimitReached,
		Message: fmt.Sprintf("tunnel limit reached: your %s plan allows %d concurrent tunnel(s)", plan, max)}
}

// countAccount counts active tunnels of accountID outside exceptSession. Caller holds mu.
func (r *Registry) countAccount(accountID, exceptSession string) int {
	n := 0
	for _, t := range r.active {
		if t.Account.ID == accountID && t.SessionID != exceptSession {
			n++
		}
	}
	return n
}

// CountAccount counts active tunnels of accountID outside exceptSession.
func (r *Registry) CountAccount(accountID, exceptSession string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.countAccount(accountID, exceptSession)
}

// ByAccount returns the active tunnels of accountID.
func (r *Registry) ByAccount(accountID string) []*Tunnel {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Tunnel
	for _, t := range r.active {
		if t.Account.ID == accountID {
			out = append(out, t)
		}
	}
	return out
}
