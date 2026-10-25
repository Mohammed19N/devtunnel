package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeResolver answers DNS lookups from maps; a missing entry is an error.
type fakeResolver struct {
	mu    sync.Mutex
	cname map[string]string
	txt   map[string][]string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{cname: map[string]string{}, txt: map[string][]string{}}
}

func (f *fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cname[host]; ok {
		return c, nil
	}
	return "", errors.New("no such host")
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.txt[name]; ok {
		return t, nil
	}
	return nil, errors.New("no such host")
}

// evictions records the calls to Config.DomainRemoved ("remove <host> <id>")
// and Config.DomainVerified ("verify <host> <id>").
type evictions struct {
	mu    sync.Mutex
	hosts []string
}

func (ev *evictions) add(kind, h string, id int64) {
	ev.mu.Lock()
	ev.hosts = append(ev.hosts, fmt.Sprintf("%s %s %d", kind, h, id))
	ev.mu.Unlock()
}
func (ev *evictions) got() []string {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return append([]string(nil), ev.hosts...)
}

func domainEnv(t *testing.T) (*testEnv, *fakeResolver) {
	e, res, _ := domainEnvEv(t)
	return e, res
}

func domainEnvEv(t *testing.T) (*testEnv, *fakeResolver, *evictions) {
	t.Helper()
	res := newFakeResolver()
	ev := &evictions{}
	return newTestEnv(t, func(c *Config) {
		c.Resolver = res
		c.DomainRemoved = func(h string, id int64) { ev.add("remove", h, id) }
		c.DomainVerified = func(h string, id int64) { ev.add("verify", h, id) }
	}), res, ev
}

func TestDomainValidation(t *testing.T) {
	e, _ := domainEnv(t)
	sc, csrf := e.signedIn(t)
	u := mustUser(t, e, "alice")
	if err := e.st.SetPlan(context.Background(), u.ID, "pro"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"1.2.3.4", "x.localhost", "localhost", "-a.com", strings.Repeat("a", 64) + ".com", "a..com", "a_b.com", "[::1]", "::1"} {
		w := e.post("/dashboard/domains", url.Values{"host": {h}}, sc, csrf)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%q = %d, want 400", h, w.Code)
		}
	}
	if list, _ := e.st.Domains(context.Background(), u.ID); len(list) != 0 {
		t.Fatalf("invalid hosts stored: %+v", list)
	}
	w := e.post("/dashboard/domains", url.Values{"host": {" Hooks.Example.COM. "}}, sc, csrf)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/dashboard/domains" {
		t.Fatalf("valid host = %d %s", w.Code, w.Body)
	}
	list, _ := e.st.Domains(context.Background(), u.ID)
	if len(list) != 1 || list[0].Host != "hooks.example.com" {
		t.Fatalf("stored %+v", list)
	}
	b := e.do("GET", "/dashboard/domains", nil, sc).Body.String()
	for _, want := range []string{"hooks.example.com", "edge.localhost", "_passway.hooks.example.com", "passway-verify=" + list[0].Token, "Pending", "CNAME", "TXT"} {
		if !strings.Contains(b, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestVerifyNeedsBothRecords(t *testing.T) {
	e, res := domainEnv(t)
	sc, csrf := e.signedIn(t)
	ctx := context.Background()
	u := mustUser(t, e, "alice")
	e.st.SetPlan(ctx, u.ID, "pro")
	e.post("/dashboard/domains", url.Values{"host": {"hooks.example.com"}}, sc, csrf)
	list, _ := e.st.Domains(ctx, u.ID)
	if len(list) != 1 {
		t.Fatalf("add failed: %+v", list)
	}
	res.cname["hooks.example.com"] = "edge.localhost."
	w := e.post("/dashboard/domains/verify", url.Values{"host": {"hooks.example.com"}}, sc, csrf)
	b := w.Body.String()
	if w.Code != http.StatusBadRequest || !strings.Contains(b, "TXT check failed") || strings.Contains(b, "CNAME check failed") {
		t.Fatalf("CNAME only = %d %s", w.Code, b)
	}
	if list, _ = e.st.Domains(ctx, u.ID); list[0].VerifiedAt != nil {
		t.Fatal("verified with the CNAME only")
	}
	// The TXT alone is not enough either.
	delete(res.cname, "hooks.example.com")
	res.txt["_passway.hooks.example.com"] = []string{"other", "passway-verify=" + list[0].Token}
	w = e.post("/dashboard/domains/verify", url.Values{"host": {"hooks.example.com"}}, sc, csrf)
	if b = w.Body.String(); w.Code != http.StatusBadRequest || !strings.Contains(b, "CNAME check failed") || strings.Contains(b, "TXT check failed") {
		t.Fatalf("TXT only = %d %s", w.Code, b)
	}
	res.cname["hooks.example.com"] = "edge.localhost."
	w = e.post("/dashboard/domains/verify", url.Values{"host": {"hooks.example.com"}}, sc, csrf)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("both records = %d %s", w.Code, w.Body)
	}
	if list, _ = e.st.Domains(ctx, u.ID); list[0].VerifiedAt == nil {
		t.Fatal("not verified")
	}
	if b := e.do("GET", "/dashboard/domains", nil, sc).Body.String(); !strings.Contains(b, "Verified") {
		t.Error("status not shown as Verified")
	}
	// Bob cannot remove alice's host.
	sc2, csrf2 := e.signedInAs(t, 43, "bob")
	if w := e.post("/dashboard/domains/remove", url.Values{"host": {"hooks.example.com"}}, sc2, csrf2); w.Code != http.StatusNotFound {
		t.Errorf("bob removing alice's host = %d, want 404", w.Code)
	}
	if w := e.post("/dashboard/domains/remove", url.Values{"host": {"hooks.example.com"}}, sc, csrf); w.Code != http.StatusSeeOther {
		t.Fatalf("remove = %d %s", w.Code, w.Body)
	}
	if list, _ = e.st.Domains(ctx, u.ID); len(list) != 0 {
		t.Fatalf("not removed: %+v", list)
	}
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var actions []string
	rows, err := db.Query(`SELECT action FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		rows.Scan(&a)
		actions = append(actions, a)
	}
	got := strings.Join(actions, ",")
	for _, want := range []string{"domain.add", "domain.verify", "domain.remove"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit missing %q in %s", want, got)
		}
	}
}

func TestDomainsPageFree(t *testing.T) {
	e, _ := domainEnv(t)
	sc, csrf := e.signedIn(t)
	b := e.do("GET", "/dashboard/domains", nil, sc).Body.String()
	if !strings.Contains(b, "Custom domains are a Pro feature") {
		t.Fatalf("no Pro copy: %s", b)
	}
	if strings.Contains(b, `name="host"`) {
		t.Fatal("claim form shown on Free")
	}
	if w := e.post("/dashboard/domains", url.Values{"host": {"hooks.example.com"}}, sc, csrf); w.Code != http.StatusForbidden {
		t.Fatalf("free add = %d, want 403", w.Code)
	}
	if !strings.Contains(e.do("GET", "/dashboard", nil, sc).Body.String(), `href="/dashboard/domains"`) {
		t.Error("dashboard does not link to custom domains")
	}
}

func TestVerifyEvictsPreviousOwner(t *testing.T) {
	e, res, ev := domainEnvEv(t)
	ctx := context.Background()
	sc, csrf := e.signedIn(t)
	alice := mustUser(t, e, "alice")
	e.st.SetPlan(ctx, alice.ID, "pro")
	e.post("/dashboard/domains", url.Values{"host": {"hooks.example.com"}}, sc, csrf)
	al, _ := e.st.Domains(ctx, alice.ID)
	res.cname["hooks.example.com"] = "edge.localhost"
	res.txt["_passway.hooks.example.com"] = []string{"passway-verify=" + al[0].Token}
	if w := e.post("/dashboard/domains/verify", url.Values{"host": {"hooks.example.com"}}, sc, csrf); w.Code != http.StatusSeeOther {
		t.Fatalf("alice verify = %d %s", w.Code, w.Body)
	}

	// The domain changes hands: bob can add it and his fresh proof wins.
	sc2, csrf2 := e.signedInAs(t, 43, "bob")
	bob := mustUser(t, e, "bob")
	e.st.SetPlan(ctx, bob.ID, "pro")
	if w := e.post("/dashboard/domains", url.Values{"host": {"hooks.example.com"}}, sc2, csrf2); w.Code != http.StatusSeeOther {
		t.Fatalf("bob add = %d %s", w.Code, w.Body)
	}
	before := len(ev.got())
	// Alice's old TXT record proves nothing for bob.
	if w := e.post("/dashboard/domains/verify", url.Values{"host": {"hooks.example.com"}}, sc2, csrf2); w.Code != http.StatusBadRequest {
		t.Fatalf("bob verify with alice's TXT = %d", w.Code)
	}
	if len(ev.got()) != before {
		t.Fatal("failed verify evicted someone")
	}
	bl, _ := e.st.Domains(ctx, bob.ID)
	res.txt["_passway.hooks.example.com"] = []string{"passway-verify=" + bl[0].Token}
	if w := e.post("/dashboard/domains/verify", url.Values{"host": {"hooks.example.com"}}, sc2, csrf2); w.Code != http.StatusSeeOther {
		t.Fatalf("bob verify = %d %s", w.Code, w.Body)
	}
	if got := ev.got(); len(got) != before+1 || got[len(got)-1] != fmt.Sprintf("verify hooks.example.com %d", bob.ID) {
		t.Fatalf("evictions = %v, want verify by bob", got)
	}
	if list, _ := e.st.Domains(ctx, alice.ID); len(list) != 0 {
		t.Fatalf("alice still has %+v", list)
	}
}

func TestRemoveDomainEvicts(t *testing.T) {
	e, res, ev := domainEnvEv(t)
	ctx := context.Background()
	sc, csrf := e.signedIn(t)
	alice := mustUser(t, e, "alice")
	e.st.SetPlan(ctx, alice.ID, "pro")
	e.post("/dashboard/domains", url.Values{"host": {"hooks.example.com"}}, sc, csrf)
	// Removing a pending row evicts nothing: it never carried a tunnel.
	if w := e.post("/dashboard/domains/remove", url.Values{"host": {"Hooks.example.com"}}, sc, csrf); w.Code != http.StatusSeeOther {
		t.Fatalf("remove pending = %d %s", w.Code, w.Body)
	}
	if got := ev.got(); len(got) != 0 {
		t.Fatalf("evictions after removing a pending row = %v", got)
	}
	// Removing a verified row evicts the remover's own tunnel only.
	e.post("/dashboard/domains", url.Values{"host": {"hooks.example.com"}}, sc, csrf)
	al, _ := e.st.Domains(ctx, alice.ID)
	res.cname["hooks.example.com"] = "edge.localhost"
	res.txt["_passway.hooks.example.com"] = []string{"passway-verify=" + al[0].Token}
	if w := e.post("/dashboard/domains/verify", url.Values{"host": {"hooks.example.com"}}, sc, csrf); w.Code != http.StatusSeeOther {
		t.Fatalf("verify = %d %s", w.Code, w.Body)
	}
	if w := e.post("/dashboard/domains/remove", url.Values{"host": {"hooks.example.com"}}, sc, csrf); w.Code != http.StatusSeeOther {
		t.Fatalf("remove verified = %d %s", w.Code, w.Body)
	}
	want := []string{fmt.Sprintf("verify hooks.example.com %d", alice.ID), fmt.Sprintf("remove hooks.example.com %d", alice.ID)}
	if got := ev.got(); !slices.Equal(got, want) {
		t.Fatalf("evictions = %v, want %v", got, want)
	}
	// Removing what you don't have evicts nothing.
	e.post("/dashboard/domains/remove", url.Values{"host": {"hooks.example.com"}}, sc, csrf)
	if got := ev.got(); len(got) != 2 {
		t.Fatalf("evictions after a 404 = %v", got)
	}
}

// A non-owner's pending add and remove of a verified host never reaches the
// eviction hooks (review I1).
func TestRemovePendingOfOwnedDomainEvictsNothing(t *testing.T) {
	e, res, ev := domainEnvEv(t)
	ctx := context.Background()
	sc, csrf := e.signedIn(t)
	alice := mustUser(t, e, "alice")
	e.st.SetPlan(ctx, alice.ID, "pro")
	e.post("/dashboard/domains", url.Values{"host": {"hooks.example.com"}}, sc, csrf)
	al, _ := e.st.Domains(ctx, alice.ID)
	res.cname["hooks.example.com"] = "edge.localhost"
	res.txt["_passway.hooks.example.com"] = []string{"passway-verify=" + al[0].Token}
	e.post("/dashboard/domains/verify", url.Values{"host": {"hooks.example.com"}}, sc, csrf)
	before := len(ev.got())

	sc2, csrf2 := e.signedInAs(t, 43, "bob")
	e.st.SetPlan(ctx, mustUser(t, e, "bob").ID, "pro")
	for i := 0; i < 3; i++ {
		if w := e.post("/dashboard/domains", url.Values{"host": {"hooks.example.com"}}, sc2, csrf2); w.Code != http.StatusSeeOther {
			t.Fatalf("bob add = %d %s", w.Code, w.Body)
		}
		if w := e.post("/dashboard/domains/remove", url.Values{"host": {"hooks.example.com"}}, sc2, csrf2); w.Code != http.StatusSeeOther {
			t.Fatalf("bob remove = %d %s", w.Code, w.Body)
		}
	}
	if got := ev.got(); len(got) != before {
		t.Fatalf("bob's pending add/remove evicted: %v", got[before:])
	}
	if list, _ := e.st.Domains(ctx, alice.ID); len(list) != 1 || list[0].VerifiedAt == nil {
		t.Fatalf("alice's domain = %+v", list)
	}
}
