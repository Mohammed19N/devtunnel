package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/mohammed19n/passway/internal/protocol"
)

const handshakeTimeout = 10 * time.Second

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
		fail(&protocol.Error{Code: protocol.CodeVersionMismatch, Message: "unsupported protocol version"})
		return
	}
	if _, err := s.cfg.Auth.Authenticate(r.Context(), hello.Token); err != nil {
		fail(&protocol.Error{Code: protocol.CodeUnauthorized, Message: "invalid token"})
		return
	}

	t := &Tunnel{session: sess}
	newTunnelProxy(t)
	replaced, err := s.registry.Claim(ClaimRequest{
		Name:          hello.Name,
		PreferredName: hello.PreferredName,
		SessionID:     hello.SessionID,
	}, t)
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
		s.log.Info("superseding stale tunnel", "name", t.Name, "session", t.SessionID)
		replaced.session.Close()
	}

	if err := protocol.WriteFrame(ctrl, protocol.HelloResponse{
		Ready: &protocol.Ready{URL: s.PublicURL(t.Name), Name: t.Name},
	}); err != nil {
		return
	}
	s.log.Info("tunnel up", "name", t.Name, "session", t.SessionID)
	defer s.log.Info("tunnel down", "name", t.Name, "session", t.SessionID)

	// EOF on the control stream ends the session.
	go func() {
		_, _ = io.Copy(io.Discard, ctrl)
		sess.Close()
	}()
	<-sess.CloseChan()
}
