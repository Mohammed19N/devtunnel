package server

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

//go:embed pages/layout.html
var pagesFS embed.FS

var pageTmpl = template.Must(template.ParseFS(pagesFS, "pages/layout.html"))

type pageKind int

const (
	pageOffline pageKind = iota
	pageRateLimited
	pageWarning
	pageBandwidth
	pageBanned
	pageBusy
)

type pageData struct {
	Host     string // r.Host as received
	Continue string // warning only: "/_passway/continue?to=<escaped path>"
	RetryIn  int    // rate-limited only: seconds
	Owner    string // warning only: GitHub login of the tunnel's owner
	Title    string // set by renderPage
	Footer   bool   // show the "Served by Passway" footer
	HomeURL  string // the footer link: <scheme>://<domain>[:port]
}

// renderPage writes the edge page for kind with its status, Cache-Control:
// no-store, anti-framing and nosniff headers, and HTML when r accepts it,
// otherwise a one-line text body.
func renderPage(w http.ResponseWriter, r *http.Request, kind pageKind, d pageData) {
	var (
		status int
		name   string
		plain  string
	)
	switch kind {
	case pageRateLimited:
		status, name, d.Title = http.StatusTooManyRequests, "ratelimited", "Too many requests"
		plain = fmt.Sprintf("rate limit exceeded; retry in %ds", d.RetryIn)
		w.Header().Set("Retry-After", strconv.Itoa(d.RetryIn))
	case pageWarning:
		status, name, d.Title = http.StatusOK, "warning", "Before you continue"
		plain = "this tunnel is served from someone's computer; send the Passway-Skip-Warning header to skip this notice"
	case pageBandwidth:
		status, name, d.Title = http.StatusPaymentRequired, "bandwidth", "Bandwidth limit reached"
		plain = "monthly bandwidth limit reached"
	case pageBanned:
		status, name, d.Title = http.StatusForbidden, "banned", "Tunnel disabled"
		plain = "this tunnel has been disabled"
	case pageBusy:
		status, name, d.Title = http.StatusServiceUnavailable, "busy", "Tunnel busy"
		plain = "tunnel is busy: too many concurrent connections"
		w.Header().Set("Retry-After", "1")
	default:
		status, name, d.Title = http.StatusBadGateway, "offline", "Tunnel offline"
		plain = fmt.Sprintf("tunnel %s is offline", d.Host)
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintln(w, plain)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = pageTmpl.ExecuteTemplate(w, name, d)
}
