package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mohammed19n/passway/internal/protocol"
	"github.com/mohammed19n/passway/internal/server"
	"github.com/mohammed19n/passway/internal/server/store"
)

const maxTokenName = 40

type tunnelRow struct {
	URL, ClientVersion, Since string
	Requests                  int64
}

type subdomainRow struct {
	Name, Host string
	IsDefault  bool
}

type tokenRow struct {
	ID                int64
	Name, Prefix      string
	Created, LastUsed string
	Revoked           bool
}

// planView is the Plan section: limits and this month's bandwidth.
type planView struct {
	Name                                   string
	Tunnels, Subdomains, Rate, Limit, Used string
	UsedBytes, LimitBytes                  int64
	Upgrade                                bool
}

// formatBytes renders n as whole MB below 1 GB and as GB with at most one
// decimal above, e.g. "300 MB", "1 GB", "1.5 GB".
func formatBytes(n int64) string {
	const gb = 1 << 30
	if n < gb {
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/gb), ".0") + " GB"
}

// relativeTime renders t as "5 minutes ago" relative to now.
func relativeTime(now, t time.Time) string {
	d := now.Sub(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour")
	}
	return plural(int(d/(24*time.Hour)), "day")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func limitText(n int) string {
	if n == 0 {
		return "unlimited"
	}
	return strconv.Itoa(n)
}

// active restricts a handler to non-waitlisted users. GETs go to /waitlist;
// POSTs get a 403.
func (d *Dashboard) active(h func(http.ResponseWriter, *http.Request, *session)) func(http.ResponseWriter, *http.Request, *session) {
	return func(w http.ResponseWriter, r *http.Request, s *session) {
		if s.User.Waitlisted {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/waitlist", http.StatusSeeOther)
				return
			}
			d.fail(w, http.StatusForbidden, "You're on the waitlist, so this isn't available yet.")
			return
		}
		h(w, r, s)
	}
}

func (d *Dashboard) baseView(s *session, title string) view {
	return view{Title: title, User: &s.User, CSRF: s.CSRF, Wide: true,
		BaseURL: d.cfg.BaseURL, Host: strings.TrimPrefix(strings.TrimPrefix(d.cfg.BaseURL, "https://"), "http://")}
}

func (d *Dashboard) subdomainRows(r *http.Request, s *session, host string) ([]subdomainRow, error) {
	subs, err := d.cfg.Store.Subdomains(r.Context(), s.User.ID)
	if err != nil {
		return nil, err
	}
	rows := make([]subdomainRow, len(subs))
	for i, sd := range subs {
		rows[i] = subdomainRow{Name: sd.Name, Host: sd.Name + "." + host, IsDefault: sd.IsDefault}
	}
	return rows, nil
}

func (d *Dashboard) handleDashboard(w http.ResponseWriter, r *http.Request, s *session) {
	if s.User.Waitlisted {
		http.Redirect(w, r, "/waitlist", http.StatusSeeOther)
		return
	}
	now := d.cfg.Now()
	v := d.baseView(s, "Dashboard")
	for _, t := range d.cfg.Tunnels(strconv.FormatInt(s.User.ID, 10)) {
		v.Tunnels = append(v.Tunnels, tunnelRow{URL: t.URL, ClientVersion: t.ClientVersion,
			Since: relativeTime(now, t.ConnectedAt), Requests: t.Requests})
	}
	plan := server.PlanByName(s.User.Plan)
	used, err := d.cfg.Store.Usage(r.Context(), s.User.ID, now.UTC().Format("2006-01"))
	if err != nil {
		d.internalError(w, "usage", err)
		return
	}
	v.Plan = planView{
		Name: capitalize(plan.Name), Tunnels: limitText(plan.MaxTunnels), Subdomains: limitText(plan.MaxSubdomains),
		Rate: limitText(plan.RateLimit), Used: formatBytes(used), UsedBytes: used, LimitBytes: plan.Bandwidth,
		Upgrade: plan.Name == server.PlanFree.Name,
	}
	v.Plan.Limit = "unlimited"
	if plan.Bandwidth > 0 {
		v.Plan.Limit = formatBytes(plan.Bandwidth)
	}
	if v.Subdomains, err = d.subdomainRows(r, s, v.Host); err != nil {
		d.internalError(w, "subdomains", err)
		return
	}
	d.render(w, http.StatusOK, "dashboard", v)
}

func (d *Dashboard) renderSubdomains(w http.ResponseWriter, r *http.Request, s *session, status int, msg string) {
	v := d.baseView(s, "Reserved subdomains")
	v.Error = msg
	var err error
	if v.Subdomains, err = d.subdomainRows(r, s, v.Host); err != nil {
		d.internalError(w, "subdomains", err)
		return
	}
	d.render(w, status, "subdomains", v)
}

func (d *Dashboard) handleSubdomains(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderSubdomains(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) handleClaim(w http.ResponseWriter, r *http.Request, s *session) {
	name := strings.ToLower(strings.TrimSpace(r.PostFormValue("name")))
	if err := server.ValidateName(name); err != nil {
		msg := err.Error()
		var pe *protocol.Error
		if errors.As(err, &pe) {
			msg = pe.Message
		}
		d.renderSubdomains(w, r, s, http.StatusBadRequest, msg)
		return
	}
	plan := server.PlanByName(s.User.Plan)
	err := d.cfg.Store.ClaimSubdomain(r.Context(), s.User.ID, name, false, plan.MaxSubdomains)
	switch {
	case errors.Is(err, store.ErrTaken):
		d.renderSubdomains(w, r, s, http.StatusConflict, "name is taken")
		return
	case errors.Is(err, store.ErrLimit):
		d.renderSubdomains(w, r, s, http.StatusForbidden,
			fmt.Sprintf("your %s plan allows %d reserved subdomain(s)", capitalize(plan.Name), plan.MaxSubdomains))
		return
	case err != nil:
		d.internalError(w, "claim subdomain", err)
		return
	}
	if err := d.cfg.Store.Audit(r.Context(), s.User.ID, "subdomain.claim", name); err != nil {
		d.internalError(w, "audit", err)
		return
	}
	http.Redirect(w, r, "/dashboard/subdomains", http.StatusSeeOther)
}

func (d *Dashboard) handleRelease(w http.ResponseWriter, r *http.Request, s *session) {
	name := strings.ToLower(strings.TrimSpace(r.PostFormValue("name")))
	err := d.cfg.Store.ReleaseSubdomain(r.Context(), s.User.ID, name)
	switch {
	case errors.Is(err, store.ErrDefault):
		d.fail(w, http.StatusBadRequest, "Your default subdomain can't be released.")
		return
	case errors.Is(err, store.ErrNotFound):
		d.fail(w, http.StatusNotFound, "You don't have that subdomain.")
		return
	case err != nil:
		d.internalError(w, "release subdomain", err)
		return
	}
	if err := d.cfg.Store.Audit(r.Context(), s.User.ID, "subdomain.release", name); err != nil {
		d.internalError(w, "audit", err)
		return
	}
	http.Redirect(w, r, "/dashboard/subdomains", http.StatusSeeOther)
}

// renderTokens shows the token page. newToken, when set, is the plaintext of
// the token this request created; it is shown here and never again.
func (d *Dashboard) renderTokens(w http.ResponseWriter, r *http.Request, s *session, status int, msg, newName, newToken string) {
	toks, err := d.cfg.Store.ListTokens(r.Context(), s.User.ID)
	if err != nil {
		d.internalError(w, "list tokens", err)
		return
	}
	v := d.baseView(s, "CLI tokens")
	v.Error, v.NewName, v.NewToken = msg, newName, newToken
	for _, t := range toks {
		row := tokenRow{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Created: t.CreatedAt.UTC().Format("2006-01-02"),
			LastUsed: "never", Revoked: t.RevokedAt != nil}
		if t.LastUsedAt != nil {
			row.LastUsed = relativeTime(d.cfg.Now(), *t.LastUsedAt)
		}
		v.Tokens = append(v.Tokens, row)
	}
	d.render(w, status, "tokens", v)
}

func (d *Dashboard) handleTokens(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderTokens(w, r, s, http.StatusOK, "", "", "")
}

func (d *Dashboard) handleCreateToken(w http.ResponseWriter, r *http.Request, s *session) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if n := utf8.RuneCountInString(name); n < 1 || n > maxTokenName {
		d.renderTokens(w, r, s, http.StatusBadRequest, fmt.Sprintf("token name must be 1-%d characters", maxTokenName), "", "")
		return
	}
	plain, tok, err := d.cfg.Store.CreateToken(r.Context(), s.User.ID, name)
	if err != nil {
		d.internalError(w, "create token", err)
		return
	}
	if err := d.cfg.Store.Audit(r.Context(), s.User.ID, "token.create", fmt.Sprintf("id=%d name=%q", tok.ID, name)); err != nil {
		d.internalError(w, "audit", err)
		return
	}
	d.renderTokens(w, r, s, http.StatusOK, "", name, plain)
}

func (d *Dashboard) handleRevokeToken(w http.ResponseWriter, r *http.Request, s *session) {
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		d.fail(w, http.StatusBadRequest, "That token id isn't valid.")
		return
	}
	switch err := d.cfg.Store.RevokeToken(r.Context(), s.User.ID, id); {
	case errors.Is(err, store.ErrNotFound):
		d.fail(w, http.StatusNotFound, "You don't have that token.")
		return
	case err != nil:
		d.internalError(w, "revoke token", err)
		return
	}
	if err := d.cfg.Store.Audit(r.Context(), s.User.ID, "token.revoke", "id="+strconv.FormatInt(id, 10)); err != nil {
		d.internalError(w, "audit", err)
		return
	}
	http.Redirect(w, r, "/dashboard/tokens", http.StatusSeeOther)
}
