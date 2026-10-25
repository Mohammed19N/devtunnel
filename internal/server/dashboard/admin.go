package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mohammed19n/passway/internal/server/store"
)

const maxAdminReports = 100

// AdminStats is the live activity shown on /admin. It mirrors server.Stats
// so the dashboard need not import the tunnel server for it.
type AdminStats struct {
	Tunnels  int
	ByPlan   map[string]int
	Requests int64
}

type adminReport struct {
	ID                                int64
	URL, Category, Details, Email, IP string
	Link                              string // URL again when it is a safe http(s) link, else ""
	Created                           string
	CanBanHost, CanBanUser            bool
}

type adminPlanCount struct {
	Plan  string
	Count int
}

type adminView struct {
	Tunnels                int
	TunnelsByPlan          []adminPlanCount
	Requests               int64
	UsersFree, UsersPro    int
	Waitlist               int
	FreeUsed, ProUsed, Cap string
	FreePercent            int
	Reports                []adminReport
	Month                  string
}

// adminOnly wraps a handler so that anyone not listed in Config.Admins gets
// the same 404 as an unknown page.
func (d *Dashboard) adminOnly(h func(http.ResponseWriter, *http.Request, *session)) func(http.ResponseWriter, *http.Request, *session) {
	return func(w http.ResponseWriter, r *http.Request, s *session) {
		if !d.isAdmin(s.User.Login) {
			d.fail(w, http.StatusNotFound, "There's nothing here.")
			return
		}
		h(w, r, s)
	}
}

func (d *Dashboard) isAdmin(login string) bool {
	for _, a := range d.cfg.Admins {
		if strings.EqualFold(strings.TrimSpace(a), login) && login != "" {
			return true
		}
	}
	return false
}

// safeLink returns raw when it parses as an http(s) URL with a host, else "".
func safeLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return raw
}

// reportTarget is what a report's URL points at: the host to ban (a label
// under the base domain, or a verified custom domain's full hostname) and
// the account that owns it. Either may be missing.
type reportTarget struct {
	Host    string // "" = nothing bannable
	OwnerID int64  // 0 = unknown
}

func (d *Dashboard) targetOf(ctx context.Context, rawURL string) (reportTarget, error) {
	var t reportTarget
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return t, nil
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == d.baseHost {
		return t, nil
	}
	if label, ok := strings.CutSuffix(host, "."+d.baseHost); ok {
		if label == "" || strings.Contains(label, ".") {
			return t, nil
		}
		t.Host = label
		switch id, err := d.cfg.Store.SubdomainOwner(ctx, label); {
		case err == nil:
			t.OwnerID = id
		case !errors.Is(err, store.ErrNotFound):
			return t, err
		}
		return t, nil
	}
	switch id, err := d.cfg.Store.VerifiedDomainOwner(ctx, host); {
	case err == nil:
		t.Host, t.OwnerID = host, id
	case !errors.Is(err, store.ErrNotFound):
		return t, err
	}
	return t, nil
}

func (d *Dashboard) handleAdmin(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	st := d.cfg.Store
	var a adminView
	if d.cfg.Stats != nil {
		stats := d.cfg.Stats()
		a.Tunnels, a.Requests = stats.Tunnels, stats.Requests
		for _, p := range []string{"free", "pro", "unlimited"} {
			if n := stats.ByPlan[p]; n > 0 {
				a.TunnelsByPlan = append(a.TunnelsByPlan, adminPlanCount{p, n})
			}
		}
	}
	var err error
	if a.UsersFree, err = st.CountUsers(ctx, "free"); err != nil {
		d.internalError(w, "admin users", err)
		return
	}
	if a.UsersPro, err = st.CountUsers(ctx, "pro"); err != nil {
		d.internalError(w, "admin users", err)
		return
	}
	if a.Waitlist, err = st.CountWaitlisted(ctx); err != nil {
		d.internalError(w, "admin waitlist", err)
		return
	}
	a.Month = d.cfg.Now().UTC().Format("2006-01")
	free, err := st.PlanUsage(ctx, "free", a.Month)
	if err != nil {
		d.internalError(w, "admin usage", err)
		return
	}
	pro, err := st.PlanUsage(ctx, "pro", a.Month)
	if err != nil {
		d.internalError(w, "admin usage", err)
		return
	}
	a.FreeUsed, a.ProUsed, a.Cap = formatBytes(free), formatBytes(pro), formatBytes(d.cfg.FreeBandwidthCap)
	if d.cfg.FreeBandwidthCap > 0 {
		a.FreePercent = int(free * 100 / d.cfg.FreeBandwidthCap)
	}
	reports, err := st.AbuseReports(ctx, true, maxAdminReports)
	if err != nil {
		d.internalError(w, "admin reports", err)
		return
	}
	now := d.cfg.Now()
	for _, rep := range reports {
		t, err := d.targetOf(ctx, rep.URL)
		if err != nil {
			d.internalError(w, "admin report target", err)
			return
		}
		a.Reports = append(a.Reports, adminReport{
			ID: rep.ID, URL: rep.URL, Category: rep.Category, Details: rep.Details, Email: rep.Email, IP: rep.IP,
			Link: safeLink(rep.URL), Created: relativeTime(now, rep.CreatedAt),
			CanBanHost: t.Host != "", CanBanUser: t.OwnerID != 0,
		})
	}
	v := d.baseView(s, "Admin")
	v.Admin = &a
	d.render(w, http.StatusOK, "admin", v)
}

// reportFromForm loads the report named by the "report" form field.
func (d *Dashboard) reportFromForm(w http.ResponseWriter, r *http.Request) (store.AbuseReport, bool) {
	id, err := strconv.ParseInt(r.PostFormValue("report"), 10, 64)
	if err != nil {
		d.fail(w, http.StatusBadRequest, "Unknown report.")
		return store.AbuseReport{}, false
	}
	list, err := d.cfg.Store.AbuseReports(r.Context(), false, 1<<30)
	if err != nil {
		d.internalError(w, "abuse reports", err)
		return store.AbuseReport{}, false
	}
	for _, rep := range list {
		if rep.ID == id {
			return rep, true
		}
	}
	d.fail(w, http.StatusNotFound, "That report doesn't exist.")
	return store.AbuseReport{}, false
}

// handleAdminBan bans the host or the owner of a report's URL. The target is
// derived from the stored report, never from the form, and the report stays
// open until it is resolved.
func (d *Dashboard) handleAdminBan(w http.ResponseWriter, r *http.Request, s *session) {
	kind := r.PostFormValue("kind")
	if kind != "host" && kind != "user" {
		d.fail(w, http.StatusBadRequest, "Choose host or user.")
		return
	}
	rep, ok := d.reportFromForm(w, r)
	if !ok {
		return
	}
	t, err := d.targetOf(r.Context(), rep.URL)
	if err != nil {
		d.internalError(w, "admin report target", err)
		return
	}
	reason := "abuse report #" + strconv.FormatInt(rep.ID, 10)
	var detail string
	switch {
	case kind == "host" && t.Host != "":
		if err = d.cfg.Store.AddBan(r.Context(), store.Ban{Kind: "host", Value: t.Host, Reason: reason}); err == nil {
			detail = t.Host
		}
	case kind == "user" && t.OwnerID != 0:
		var u store.User
		if u, err = d.cfg.Store.UserByID(r.Context(), t.OwnerID); err == nil {
			if err = d.cfg.Store.BanUser(r.Context(), u, reason); err == nil {
				detail = "id:" + strconv.FormatInt(u.ID, 10) + " @" + u.Login
			}
		}
	default:
		d.fail(w, http.StatusBadRequest, "That report has no "+kind+" that can be banned.")
		return
	}
	if err != nil {
		d.internalError(w, "admin ban", err)
		return
	}
	if d.cfg.PollBans != nil {
		d.cfg.PollBans()
	}
	if err := d.cfg.Store.Audit(r.Context(), s.User.ID, "admin.ban."+kind, detail+" ("+reason+")"); err != nil {
		d.internalError(w, "audit", err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (d *Dashboard) handleAdminResolve(w http.ResponseWriter, r *http.Request, s *session) {
	id, err := strconv.ParseInt(r.PostFormValue("report"), 10, 64)
	if err != nil {
		d.fail(w, http.StatusBadRequest, "Unknown report.")
		return
	}
	switch err := d.cfg.Store.ResolveAbuseReport(r.Context(), id); {
	case errors.Is(err, store.ErrNotFound):
		d.fail(w, http.StatusNotFound, "That report doesn't exist.")
		return
	case err != nil:
		d.internalError(w, "resolve report", err)
		return
	}
	if err := d.cfg.Store.Audit(r.Context(), s.User.ID, "admin.resolve", "abuse report #"+strconv.FormatInt(id, 10)); err != nil {
		d.internalError(w, "audit", err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
