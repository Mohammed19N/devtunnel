package dashboard

import (
	"math"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mohammed19n/passway/internal/server/store"
)

const (
	abusePerHour    = 5    // reports per IP per hour
	maxAbuseDetails = 5000 // characters
	maxAbuseURL     = 2048
	maxAbuseEmail   = 254
)

// abuseCategories are the form's categories in display order.
var abuseCategories = []struct{ Value, Label string }{
	{"phishing", "Phishing"}, {"malware", "Malware"}, {"spam", "Spam"}, {"other", "Other"},
}

// abuseFormState is the abuse form's state when it is shown again with an error.
type abuseFormState struct {
	URL, Category, Details, Email string
	Error                         string
}

// Categories lets the template mark the selected option.
func (abuseFormState) Categories() []struct{ Value, Label string } { return abuseCategories }

// validate checks f and returns a message for the first problem, or "".
func (f abuseFormState) validate() string {
	u, err := url.Parse(f.URL)
	switch {
	case f.URL == "":
		return "Enter the URL you are reporting."
	case len(f.URL) > maxAbuseURL || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return "Enter the full URL, starting with http:// or https://."
	}
	ok := false
	for _, c := range abuseCategories {
		ok = ok || c.Value == f.Category
	}
	if !ok {
		return "Choose a category."
	}
	if strings.TrimSpace(f.Details) == "" {
		return "Describe what you found."
	}
	if utf8.RuneCountInString(f.Details) > maxAbuseDetails {
		return "Keep the details under " + strconv.Itoa(maxAbuseDetails) + " characters."
	}
	if f.Email != "" {
		if a, err := mail.ParseAddress(f.Email); err != nil || a.Address != f.Email || len(f.Email) > maxAbuseEmail {
			return "Enter a valid email address, or leave it empty."
		}
	}
	return ""
}

// remoteIP is r.RemoteAddr without the port (the real client behind the
// trusted proxy; see the PROXY protocol listener).
func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// handleAbuse takes a public abuse report: same origin only, a honeypot field
// that must stay empty, and at most abusePerHour stored reports per IP.
func (d *Dashboard) handleAbuse(w http.ResponseWriter, r *http.Request) {
	if !d.hasSameOrigin(r) {
		d.fail(w, http.StatusForbidden, "This form expired. Go back, reload the page and try again.")
		return
	}
	if err := r.ParseForm(); err != nil {
		d.fail(w, http.StatusBadRequest, "That form couldn't be read. Go back and try again.")
		return
	}
	if r.PostFormValue("website") != "" { // only bots fill the hidden field
		http.Redirect(w, r, "/abuse?sent=1", http.StatusSeeOther)
		return
	}
	f := abuseFormState{
		URL:      strings.TrimSpace(r.PostFormValue("url")),
		Category: r.PostFormValue("category"),
		Details:  strings.TrimSpace(r.PostFormValue("details")),
		Email:    strings.TrimSpace(r.PostFormValue("email")),
	}
	if f.Error = f.validate(); f.Error != "" {
		d.renderAbuse(w, r, http.StatusBadRequest, f)
		return
	}
	ip := remoteIP(r)
	if ok, retry := d.abuseLimiter.AllowN("abuse:"+ip, abusePerHour); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
		f.Error = "You have sent several reports in the last hour. Please wait a while, or email " + d.cfg.AbuseEmail + "."
		d.renderAbuse(w, r, http.StatusTooManyRequests, f)
		return
	}
	id, err := d.cfg.Store.AddAbuseReport(r.Context(), store.AbuseReport{
		URL: f.URL, Category: f.Category, Details: f.Details, Email: f.Email, IP: ip,
	})
	if err != nil {
		d.internalError(w, "add abuse report", err)
		return
	}
	d.log.Warn("abuse report", "id", id, "url", f.URL, "category", f.Category, "ip", ip)
	http.Redirect(w, r, "/abuse?sent=1", http.StatusSeeOther)
}

func (d *Dashboard) renderAbuse(w http.ResponseWriter, r *http.Request, status int, f abuseFormState) {
	v, ok := d.siteView(w, r, seoAbuse)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	v.Abuse = f
	d.render(w, status, "abuse", v)
}
