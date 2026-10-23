package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const (
	warnCookie   = "passway_warned"
	skipHeader   = "Passway-Skip-Warning"
	continuePath = "/_passway/continue"
)

// warnToken is the warned-cookie value for host (lowercased, port included):
// the first 16 bytes of HMAC-SHA256(secret, host), hex encoded. The tunnel's
// app cannot mint it without the server secret.
func warnToken(secret []byte, host string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(strings.ToLower(host)))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// isNavigation reports whether r is a browser loading a page. Sec-Fetch-*
// decides when present (so an auto-submitted form POST counts); otherwise a
// GET or HEAD that accepts HTML does.
func isNavigation(r *http.Request) bool {
	mode, dest := r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Dest")
	if mode != "" || dest != "" {
		return mode == "navigate" || dest == "document"
	}
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

// needsWarning reports whether r is a browser navigation that has not yet
// acknowledged the interstitial.
func needsWarning(r *http.Request, secret []byte) bool {
	if !isNavigation(r) {
		return false
	}
	if _, ok := r.Header[http.CanonicalHeaderKey(skipHeader)]; ok {
		return false
	}
	if c, err := r.Cookie(warnCookie); err == nil && hmac.Equal([]byte(c.Value), []byte(warnToken(secret, r.Host))) {
		return false
	}
	return !strings.HasPrefix(r.URL.Path, "/_passway/")
}

// handleContinue sets the warned cookie and redirects to a safe local path.
// secure adds the Secure attribute (public scheme is https).
func handleContinue(w http.ResponseWriter, r *http.Request, secret []byte, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     warnCookie,
		Value:    warnToken(secret, r.Host),
		Path:     "/",
		MaxAge:   7 * 24 * 60 * 60,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	// Set Location directly: http.Redirect would path.Clean it after validation.
	w.Header().Set("Location", safeReturnPath(r.URL.Query().Get("to")))
	w.WriteHeader(http.StatusSeeOther)
}

// safeReturnPath returns to only if it is a local absolute path, else "/".
// Backslashes and control characters are rejected anywhere, raw or
// percent-encoded, since browsers treat "/\host" like "//host".
func safeReturnPath(to string) string {
	if !localPath(to) {
		return "/"
	}
	dec, err := url.PathUnescape(to)
	if err != nil || strings.ContainsAny(dec, `\`) || hasCtl(dec) || strings.ContainsAny(to, `\`) || hasCtl(to) {
		return "/"
	}
	u, err := url.Parse(to)
	if err != nil || u.Scheme != "" || u.Host != "" || !localPath(path.Clean(u.Path)) {
		return "/"
	}
	return to
}

// localPath reports whether p starts with a single "/" not followed by "/" or "\".
func localPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.HasPrefix(p, `/\`)
}

func hasCtl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// stripWarnCookie removes the warned cookie from the Cookie headers in h, so
// the tunnel's app never sees its value.
func stripWarnCookie(h http.Header) {
	var out []string
	for _, line := range h["Cookie"] {
		var keep []string
		for _, part := range strings.Split(line, ";") {
			name, _, _ := strings.Cut(part, "=")
			if part = strings.TrimSpace(part); part != "" && strings.TrimSpace(name) != warnCookie {
				keep = append(keep, part)
			}
		}
		if len(keep) > 0 {
			out = append(out, strings.Join(keep, "; "))
		}
	}
	if len(out) == 0 {
		h.Del("Cookie")
		return
	}
	h["Cookie"] = out
}

// dropWarnSetCookie removes any Set-Cookie in h that would set the warned cookie.
func dropWarnSetCookie(h http.Header) {
	var out []string
	for _, v := range h["Set-Cookie"] {
		if name, _, _ := strings.Cut(v, "="); strings.TrimSpace(name) != warnCookie {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		h.Del("Set-Cookie")
		return
	}
	h["Set-Cookie"] = out
}
