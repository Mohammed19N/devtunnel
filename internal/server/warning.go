package server

import (
	"net/http"
	"net/url"
	"strings"
)

const (
	warnCookie   = "passway_warned"
	skipHeader   = "Passway-Skip-Warning"
	continuePath = "/_passway/continue"
)

// needsWarning reports whether r is a browser navigation that has not yet
// acknowledged the interstitial.
func needsWarning(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false
	}
	if _, ok := r.Header[http.CanonicalHeaderKey(skipHeader)]; ok {
		return false
	}
	if c, err := r.Cookie(warnCookie); err == nil && c.Value != "" {
		return false
	}
	return !strings.HasPrefix(r.URL.Path, "/_passway/")
}

// handleContinue sets the warned cookie and redirects to a safe local path.
func handleContinue(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     warnCookie,
		Value:    "1",
		Path:     "/",
		MaxAge:   7 * 24 * 60 * 60,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, safeReturnPath(r.URL.Query().Get("to")), http.StatusSeeOther)
}

// safeReturnPath returns to only if it is a local absolute path, else "/".
func safeReturnPath(to string) string {
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.HasPrefix(to, `/\`) {
		return "/"
	}
	u, err := url.Parse(to)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return to
}
