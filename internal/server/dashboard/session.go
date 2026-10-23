package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/mohammed19n/passway/internal/server/store"
)

const (
	sessionTTL = 30 * 24 * time.Hour
	stateTTL   = 10 * time.Minute
)

// session is a signed-in request's session and user.
type session struct {
	ID string
	store.Session
	User store.User
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return base64.RawURLEncoding.EncodeToString(b)
}

// setCookie writes a host-only cookie; maxAge < 0 deletes it.
func (d *Dashboard) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   d.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// readCookie returns the value of the cookie called name. Only that exact
// name is read (on https the __Host- one, never the unprefixed fallback). If
// the request carries it more than once, one copy was set by someone else
// (a tunnel subdomain with Domain=<base>), so none is trusted.
func readCookie(r *http.Request, name string) (string, bool) {
	var val string
	n := 0
	for _, c := range r.Cookies() {
		if c.Name == name {
			val = c.Value
			n++
		}
	}
	return val, n == 1 && val != ""
}

// currentSession returns the request's session, or nil when signed out. An
// unknown or expired session cookie is cleared.
func (d *Dashboard) currentSession(w http.ResponseWriter, r *http.Request) (*session, error) {
	id, ok := readCookie(r, d.sessionCookie)
	if !ok {
		return nil, nil
	}
	ctx := r.Context()
	sess, err := d.cfg.Store.SessionByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		d.setCookie(w, d.sessionCookie, "", -1)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u, err := d.cfg.Store.UserByID(ctx, sess.UserID)
	if errors.Is(err, store.ErrNotFound) {
		d.setCookie(w, d.sessionCookie, "", -1)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session{ID: id, Session: sess, User: u}, nil
}

// startSession creates a session for userID and sets its cookie.
func (d *Dashboard) startSession(ctx context.Context, w http.ResponseWriter, userID int64) error {
	id, _, err := d.cfg.Store.CreateSession(ctx, userID, sessionTTL)
	if err != nil {
		return err
	}
	d.setCookie(w, d.sessionCookie, id, int(sessionTTL/time.Second))
	return nil
}

// csrfOK reports whether the form's csrf field matches the session's token.
func csrfOK(r *http.Request, sess *session) bool {
	got := r.PostFormValue("csrf")
	return got != "" && sess.CSRF != "" && hmac.Equal([]byte(got), []byte(sess.CSRF))
}

// sameOrigin reports whether the request's Origin (or, without one, its
// Referer) is BaseURL's origin. Requests carrying neither pass.
func (d *Dashboard) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == d.origin
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && u.Scheme+"://"+u.Host == d.origin
	}
	return true
}

// page wraps a GET handler that needs a session; signed-out requests go to /.
func (d *Dashboard) page(h func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := d.currentSession(w, r)
		if err != nil {
			d.internalError(w, "session", err)
			return
		}
		if s == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		h(w, r, s)
	}
}

// post wraps a POST handler: it needs a session, a same-origin request and
// the session's CSRF token, otherwise 403.
func (d *Dashboard) post(h func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := d.currentSession(w, r)
		if err != nil {
			d.internalError(w, "session", err)
			return
		}
		if s == nil || !d.sameOrigin(r) || !csrfOK(r, s) {
			d.fail(w, http.StatusForbidden, "This form expired. Go back, reload the page and try again.")
			return
		}
		h(w, r, s)
	}
}

func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request, s *session) {
	if err := d.cfg.Store.DeleteSession(r.Context(), s.ID); err != nil {
		d.internalError(w, "delete session", err)
		return
	}
	d.setCookie(w, d.sessionCookie, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
