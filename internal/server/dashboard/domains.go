package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mohammed19n/passway/internal/protocol"
	"github.com/mohammed19n/passway/internal/server"
	"github.com/mohammed19n/passway/internal/server/store"
)

// Resolver is the DNS lookups the custom-domain check needs; *net.Resolver
// implements it.
type Resolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

const (
	dnsTimeout  = 10 * time.Second
	txtPrefix   = "_passway."
	tokenPrefix = "passway-verify="
)

type domainRow struct {
	Host, TXTName, TXTValue string
	Verified                bool
}

func (d *Dashboard) renderDomains(w http.ResponseWriter, r *http.Request, s *session, status int, msg string) {
	doms, err := d.cfg.Store.Domains(r.Context(), s.User.ID)
	if err != nil {
		d.internalError(w, "domains", err)
		return
	}
	v := d.baseView(s, "Custom domains")
	v.Error = msg
	v.EdgeHost = d.cfg.EdgeHost
	for _, dm := range doms {
		v.Domains = append(v.Domains, domainRow{Host: dm.Host, TXTName: txtPrefix + dm.Host,
			TXTValue: tokenPrefix + dm.Token, Verified: dm.VerifiedAt != nil})
	}
	plan := server.PlanByName(s.User.Plan)
	v.DomainMax = plan.MaxCustomDomains
	v.DomainFull = len(doms) >= plan.MaxCustomDomains
	d.render(w, status, "domains", v)
}

func (d *Dashboard) handleDomains(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderDomains(w, r, s, http.StatusOK, "")
}

func (d *Dashboard) handleAddDomain(w http.ResponseWriter, r *http.Request, s *session) {
	host, err := server.NormalizeDomain(r.PostFormValue("host"), d.baseHost)
	if err != nil {
		msg := err.Error()
		var pe *protocol.Error
		if errors.As(err, &pe) {
			msg = pe.Message
		}
		d.renderDomains(w, r, s, http.StatusBadRequest, msg)
		return
	}
	plan := server.PlanByName(s.User.Plan)
	_, err = d.cfg.Store.AddDomain(r.Context(), s.User.ID, host, plan.MaxCustomDomains)
	switch {
	case errors.Is(err, store.ErrLimit):
		msg := "Custom domains are a Pro feature."
		if plan.MaxCustomDomains > 0 {
			msg = fmt.Sprintf("your %s plan allows %d custom domain(s)", capitalize(plan.Name), plan.MaxCustomDomains)
		}
		d.renderDomains(w, r, s, http.StatusForbidden, msg)
		return
	case err != nil:
		d.internalError(w, "add domain", err)
		return
	}
	if err := d.cfg.Store.Audit(r.Context(), s.User.ID, "domain.add", host); err != nil {
		d.internalError(w, "audit", err)
		return
	}
	http.Redirect(w, r, "/dashboard/domains", http.StatusSeeOther)
}

// checkDNS returns the failed checks for host: its CNAME must be EdgeHost and
// the TXT records at _passway.<host> must include passway-verify=<token>.
// The token is per user and per claim, so DNS someone else set up for their
// own claim never verifies yours.
func (d *Dashboard) checkDNS(ctx context.Context, host, token string) []string {
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	var failed []string
	cname, err := d.cfg.Resolver.LookupCNAME(ctx, host)
	if err != nil || strings.TrimSuffix(strings.ToLower(cname), ".") != d.cfg.EdgeHost {
		failed = append(failed, fmt.Sprintf("CNAME check failed: %s must be a CNAME record pointing to %s", host, d.cfg.EdgeHost))
	}
	txts, err := d.cfg.Resolver.LookupTXT(ctx, txtPrefix+host)
	want := tokenPrefix + token
	if err != nil || !slices.ContainsFunc(txts, func(t string) bool { return strings.TrimSpace(t) == want }) {
		failed = append(failed, fmt.Sprintf("TXT check failed: %s%s must have a TXT record %s", txtPrefix, host, want))
	}
	return failed
}

func (d *Dashboard) handleVerifyDomain(w http.ResponseWriter, r *http.Request, s *session) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.PostFormValue("host"))), ".")
	doms, err := d.cfg.Store.Domains(r.Context(), s.User.ID)
	if err != nil {
		d.internalError(w, "domains", err)
		return
	}
	i := slices.IndexFunc(doms, func(dm store.CustomDomain) bool { return dm.Host == host })
	if i < 0 {
		d.fail(w, http.StatusNotFound, "You haven't added that domain.")
		return
	}
	if doms[i].VerifiedAt != nil {
		http.Redirect(w, r, "/dashboard/domains", http.StatusSeeOther)
		return
	}
	if failed := d.checkDNS(r.Context(), host, doms[i].Token); len(failed) > 0 {
		d.renderDomains(w, r, s, http.StatusBadRequest, strings.Join(failed, ". ")+".")
		return
	}
	switch err := d.cfg.Store.VerifyDomain(r.Context(), s.User.ID, host); {
	case errors.Is(err, store.ErrTaken), errors.Is(err, store.ErrNotFound):
		// Another account verified it between our DNS check and now.
		d.renderDomains(w, r, s, http.StatusConflict, fmt.Sprintf("%s was just verified by another account", host))
		return
	case err != nil:
		d.internalError(w, "verify domain", err)
		return
	}
	// The fresh proof wins: close the previous owner's tunnel on the host.
	d.cfg.DomainEvicted(host)
	if err := d.cfg.Store.Audit(r.Context(), s.User.ID, "domain.verify", host); err != nil {
		d.internalError(w, "audit", err)
		return
	}
	http.Redirect(w, r, "/dashboard/domains", http.StatusSeeOther)
}

func (d *Dashboard) handleRemoveDomain(w http.ResponseWriter, r *http.Request, s *session) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.PostFormValue("host"))), ".")
	switch err := d.cfg.Store.RemoveDomain(r.Context(), s.User.ID, host); {
	case errors.Is(err, store.ErrNotFound):
		d.fail(w, http.StatusNotFound, "You don't have that domain.")
		return
	case err != nil:
		d.internalError(w, "remove domain", err)
		return
	}
	d.cfg.DomainEvicted(host)
	if err := d.cfg.Store.Audit(r.Context(), s.User.ID, "domain.remove", host); err != nil {
		d.internalError(w, "audit", err)
		return
	}
	http.Redirect(w, r, "/dashboard/domains", http.StatusSeeOther)
}
