package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/mohammed19n/passway/internal/protocol"
)

const (
	handshakeTimeout = 10 * time.Second
	minSessionIDLen  = 16
)

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already replied
	}
	c.SetReadLimit(-1) // yamux frames exceed the default 32 KB limit
	conn := websocket.NetConn(r.Context(), c, websocket.MessageBinary)

	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.LogOutput = io.Discard
	cfg.AcceptBacklog = 16
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		conn.Close()
		return
	}
	defer sess.Close()

	actx, cancel := context.WithTimeout(r.Context(), handshakeTimeout)
	ctrl, err := sess.AcceptStreamWithContext(actx)
	cancel()
	if err != nil {
		s.log.Debug("control stream not opened", "err", err)
		return
	}
	_ = ctrl.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var hello protocol.Hello
	if err := protocol.ReadFrame(ctrl, &hello); err != nil {
		s.log.Debug("reading hello failed", "err", err)
		return
	}
	_ = ctrl.SetReadDeadline(time.Time{})

	fail := func(e *protocol.Error) {
		_ = protocol.WriteFrame(ctrl, protocol.HelloResponse{Error: e})
	}

	if hello.Version != protocol.Version {
		fail(&protocol.Error{Code: protocol.CodeVersionMismatch, Message: "unsupported protocol version; please upgrade passway"})
		return
	}
	acc, err := s.cfg.Auth.Authenticate(r.Context(), hello.Token)
	if err != nil {
		var pe *protocol.Error
		switch {
		case errors.As(err, &pe):
			fail(pe)
		case errors.Is(err, ErrInvalidToken):
			fail(&protocol.Error{Code: protocol.CodeUnauthorized, Message: "invalid token"})
		default:
			s.log.Warn("authenticate failed", "err", err)
			fail(&protocol.Error{Code: protocol.CodeInternal, Message: "temporary server error; retrying"})
		}
		return
	}

	if len(hello.SessionID) < minSessionIDLen {
		fail(&protocol.Error{Code: protocol.CodeInternal, Message: "invalid session id"})
		return
	}

	t := &Tunnel{session: sess, Account: acc, ClientVersion: hello.ClientVersion, ConnectedAt: time.Now()}
	newTunnelProxy(t, s.cfg.MaxConnsPerTunnel, s.log)
	var replaced *Tunnel
	if s.cfg.Directory != nil {
		replaced, err = s.claimCloud(r.Context(), acc, hello, t)
	} else {
		replaced, err = s.registry.Claim(ClaimRequest{
			Name:          hello.Name,
			PreferredName: hello.PreferredName,
			SessionID:     hello.SessionID,
		}, t)
	}
	if err != nil {
		var pe *protocol.Error
		if !errors.As(err, &pe) {
			pe = &protocol.Error{Code: protocol.CodeInternal, Message: err.Error()}
		}
		fail(pe)
		return
	}
	defer func() {
		s.registry.Release(t)
		t.transport.CloseIdleConnections()
	}()
	if replaced != nil && replaced.session != nil {
		s.log.Info("superseding stale tunnel", "name", t.Name, "session", t.SessionID[:8])
		replaced.session.Close()
	}

	ready := &protocol.Ready{URL: s.PublicURL(t.Name), Name: t.Name}
	if s.cfg.Directory != nil {
		ready.Plan, ready.RateLimit, ready.Bandwidth = acc.Plan.Name, acc.Plan.RateLimit, acc.Plan.Bandwidth
	}
	if err := protocol.WriteFrame(ctrl, protocol.HelloResponse{Ready: ready}); err != nil {
		return
	}
	s.log.Info("tunnel up", "name", t.Name, "session", t.SessionID[:8], "account", acc.ID, "login", acc.Login)
	defer s.log.Info("tunnel down", "name", t.Name, "session", t.SessionID[:8], "account", acc.ID, "login", acc.Login)

	// Clients never open streams after the handshake; reject any that try so
	// they cannot pile up in yamux's accept backlog.
	go func() {
		for {
			st, err := sess.Accept()
			if err != nil {
				return
			}
			st.Close()
		}
	}()

	// EOF on the control stream ends the session.
	go func() {
		_, _ = io.Copy(io.Discard, ctrl)
		sess.Close()
	}()
	<-sess.CloseChan()
}

// transient wraps a directory failure as a retryable error.
func (s *Server) transient(what string, err error) error {
	s.log.Warn("directory "+what+" failed", "err", err)
	return &protocol.Error{Code: protocol.CodeInternal, Message: "temporary server error; retrying"}
}

// claimCloud resolves and claims a name for acc using the account directory.
// The tunnel limit is enforced inside Registry.Claim so concurrent handshakes
// cannot overshoot it.
func (s *Server) claimCloud(ctx context.Context, acc Account, hello protocol.Hello, t *Tunnel) (*Tunnel, error) {
	dir := s.cfg.Directory
	banned := &protocol.Error{Code: protocol.CodeBanned, Message: "account suspended"}
	if b, err := dir.Banned(ctx, acc, ""); err != nil {
		return nil, s.transient("ban check", err)
	} else if b {
		return nil, banned
	}
	req := ClaimRequest{
		SessionID:  hello.SessionID,
		AccountID:  acc.ID,
		PlanName:   acc.Plan.Name,
		MaxTunnels: acc.Plan.MaxTunnels,
	}

	if hello.Name != "" {
		owned, err := dir.Owns(ctx, acc, hello.Name)
		if err != nil {
			return nil, s.transient("ownership check", err)
		}
		if !owned {
			return nil, &protocol.Error{Code: protocol.CodeNameNotOwned, Message: fmt.Sprintf("you don't own %q; claim it in the dashboard", hello.Name)}
		}
		if b, err := dir.Banned(ctx, acc, hello.Name); err != nil {
			return nil, s.transient("ban check", err)
		} else if b {
			return nil, banned
		}
		req.Name = hello.Name
		return s.registry.Claim(req, t)
	}

	names, err := dir.Names(ctx, acc)
	if err != nil {
		return nil, s.transient("names", err)
	}
	cands := make([]string, 0, len(names))
	if p := hello.PreferredName; p != "" && slices.Contains(names, p) {
		cands = append(cands, p)
	}
	for _, n := range names {
		if n != hello.PreferredName || len(cands) == 0 || cands[0] != n {
			cands = append(cands, n)
		}
	}
	for _, name := range cands {
		if err := ValidateName(name); err != nil {
			s.log.Debug("skipping invalid owned name", "name", name, "err", err)
			continue
		}
		if b, err := dir.Banned(ctx, acc, name); err != nil {
			return nil, s.transient("ban check", err)
		} else if b {
			s.log.Debug("skipping banned name", "name", name)
			continue
		}
		req.Name = name
		replaced, err := s.registry.Claim(req, t)
		var pe *protocol.Error
		if errors.As(err, &pe) && pe.Code == protocol.CodeNameTaken {
			continue
		}
		return replaced, err
	}
	return nil, &protocol.Error{Code: protocol.CodeNameTaken, Message: "all your subdomains are in use; use --name or upgrade"}
}
