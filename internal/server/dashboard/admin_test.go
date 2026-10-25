package dashboard

import (
	"database/sql"
	"strconv"

	"context"
	"github.com/mohammed19n/passway/internal/server/store"
	_ "modernc.org/sqlite"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type adminEnv struct {
	*testEnv
	polls atomic.Int32
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	ae := &adminEnv{}
	ae.testEnv = newTestEnv(t, func(c *Config) {
		c.AdminGitHubIDs = []int64{42}
		c.Now = func() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC) }
		c.FreeBandwidthCap = 1000
		c.Stats = func() AdminStats {
			return AdminStats{Tunnels: 3, ByPlan: map[string]int{"free": 2, "pro": 1}, Requests: 4242}
		}
		c.PollBans = func() { ae.polls.Add(1) }
	})
	return ae
}

func TestAdminPageNonAdmin404(t *testing.T) {
	e := newAdminEnv(t)
	sc, csrf := e.signedInAs(t, 9, "mallory")
	if w := e.do("GET", "/admin", nil, sc); w.Code != http.StatusNotFound {
		t.Fatalf("GET /admin as non-admin = %d, want 404", w.Code)
	}
	for _, p := range []string{"/admin/ban", "/admin/resolve"} {
		if w := e.post(p, url.Values{"report": {"1"}, "kind": {"host"}}, sc, csrf); w.Code != http.StatusNotFound {
			t.Errorf("POST %s as non-admin = %d, want 404", p, w.Code)
		}
	}
	if w := e.do("GET", "/admin", nil); w.Code == http.StatusOK {
		t.Fatalf("signed-out GET /admin = %d", w.Code)
	}
}

func TestAdminKeyedOnGitHubID(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	sc, _ := e.signedInAs(t, 42, "alice")
	if w := e.do("GET", "/admin", nil, sc); w.Code != http.StatusOK {
		t.Fatalf("admin by id = %d", w.Code)
	}
	// alice renames; someone else registers the old login with another GitHub ID.
	if _, _, err := e.st.UpsertGitHubUser(ctx, store.User{GitHubID: 42, Login: "alice-new"}); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "/admin", nil, sc); w.Code != http.StatusOK {
		t.Fatalf("renamed admin = %d, want 200", w.Code)
	}
	sc2, csrf2 := e.signedInAs(t, 999, "alice")
	if w := e.do("GET", "/admin", nil, sc2); w.Code != http.StatusNotFound {
		t.Fatalf("login squatter = %d, want 404", w.Code)
	}
	if w := e.post("/admin/resolve", url.Values{"report": {"1"}}, sc2, csrf2); w.Code != http.StatusNotFound {
		t.Fatalf("squatter POST = %d, want 404", w.Code)
	}
}

func TestAdminPageShowsStats(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	sc, _ := e.signedInAs(t, 42, "alice") // case-insensitive match with "Alice"
	bob := mkUserFor(t, e.testEnv, 43, "bob")
	if err := e.st.SetPlan(ctx, bob, "pro"); err != nil {
		t.Fatal(err)
	}
	wl := mkUserFor(t, e.testEnv, 44, "wait")
	if err := e.st.SetWaitlisted(ctx, wl, true); err != nil {
		t.Fatal(err)
	}
	alice := mustUser(t, e.testEnv, "alice")
	if err := e.st.AddUsage(ctx, alice.ID, "2026-10", 250); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/admin", nil, sc)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin = %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{"4242", "noindex", "25%", `data-stat="tunnels">3<`, `data-stat="waitlist">1<`,
		`data-stat="users-free">1<`, `data-stat="users-pro">1<`} {
		if !strings.Contains(body, want) {
			t.Errorf("admin page lacks %q", want)
		}
	}
}

func addReport(t *testing.T, e *testEnv, u, details string) int64 {
	t.Helper()
	id, err := e.st.AddAbuseReport(context.Background(), abuseRow(u, details))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAdminBanFromReport(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	sc, csrf := e.signedInAs(t, 42, "alice")
	bob := mkUserFor(t, e.testEnv, 43, "bob")
	if err := e.st.ClaimSubdomain(ctx, bob, "evil-site", true, 5); err != nil {
		t.Fatal(err)
	}
	id := addReport(t, e.testEnv, "https://evil-site.localhost/login", "phish")

	page := e.do("GET", "/admin", nil, sc).Body.String()
	if !strings.Contains(page, "Ban host") || !strings.Contains(page, "Ban user") || !strings.Contains(page, "Resolve") {
		t.Fatalf("report buttons missing: %s", page)
	}

	w := e.post("/admin/ban", url.Values{"report": {itoa(id)}, "kind": {"host"}}, sc, csrf)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("ban host = %d %s", w.Code, w.Body)
	}
	if b, _ := e.st.IsBanned(ctx, "host", "evil-site"); !b {
		t.Fatal("host label not banned")
	}
	if e.polls.Load() != 1 {
		t.Fatalf("PollBans calls = %d, want 1", e.polls.Load())
	}
	w = e.post("/admin/ban", url.Values{"report": {itoa(id)}, "kind": {"user"}}, sc, csrf)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("ban user = %d %s", w.Code, w.Body)
	}
	bobU, _ := e.st.UserByID(ctx, bob)
	if b, _ := e.st.IsUserBanned(ctx, bobU); !b {
		t.Fatal("owner not banned")
	}
	if e.polls.Load() != 2 {
		t.Fatalf("PollBans calls = %d, want 2", e.polls.Load())
	}
	open, _ := e.st.AbuseReports(ctx, true, 10)
	if len(open) != 1 {
		t.Fatalf("report resolved by banning: %+v", open)
	}

	// Resolve closes it.
	if w := e.post("/admin/resolve", url.Values{"report": {itoa(id)}}, sc, csrf); w.Code != http.StatusSeeOther {
		t.Fatalf("resolve = %d %s", w.Code, w.Body)
	}
	if open, _ := e.st.AbuseReports(ctx, true, 10); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
	if e.polls.Load() != 2 {
		t.Fatalf("resolve polled bans")
	}
	var n int
	if err := rawDB(t, e.dbPath).QueryRow(`SELECT count(*) FROM audit_log WHERE action LIKE 'admin.%'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("admin audit rows = %d, %v; want 3", n, err)
	}
}

func TestAdminBanCustomDomainReport(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	sc, csrf := e.signedInAs(t, 42, "alice")
	bob := mkUserFor(t, e.testEnv, 43, "bob")
	if _, err := e.st.AddDomain(ctx, bob, "bad.example.com", 2); err != nil {
		t.Fatal(err)
	}
	if err := e.st.VerifyDomain(ctx, bob, "bad.example.com"); err != nil {
		t.Fatal(err)
	}
	id := addReport(t, e.testEnv, "https://Bad.Example.com:8443/x", "phish")
	if w := e.post("/admin/ban", url.Values{"report": {itoa(id)}, "kind": {"host"}}, sc, csrf); w.Code != http.StatusSeeOther {
		t.Fatalf("ban = %d %s", w.Code, w.Body)
	}
	if b, _ := e.st.IsBanned(ctx, "host", "bad.example.com"); !b {
		t.Fatal("custom domain not banned")
	}
	if w := e.post("/admin/ban", url.Values{"report": {itoa(id)}, "kind": {"user"}}, sc, csrf); w.Code != http.StatusSeeOther {
		t.Fatalf("ban user = %d", w.Code)
	}
	bobU, _ := e.st.UserByID(ctx, bob)
	if b, _ := e.st.IsUserBanned(ctx, bobU); !b {
		t.Fatal("custom-domain owner not banned")
	}
}

func TestAdminBanUnbannableReport(t *testing.T) {
	e := newAdminEnv(t)
	sc, csrf := e.signedInAs(t, 42, "alice")
	id := addReport(t, e.testEnv, "https://unrelated.example.org/", "x")
	for _, kind := range []string{"host", "user", "bogus"} {
		if w := e.post("/admin/ban", url.Values{"report": {itoa(id)}, "kind": {kind}}, sc, csrf); w.Code != http.StatusBadRequest {
			t.Errorf("ban %s on unrelated host = %d, want 400", kind, w.Code)
		}
	}
	if e.polls.Load() != 0 {
		t.Fatal("polled bans for a refused ban")
	}
}

func TestAdminPostsRequireCSRF(t *testing.T) {
	e := newAdminEnv(t)
	sc, csrf := e.signedInAs(t, 42, "alice")
	for _, p := range []string{"/admin/ban", "/admin/resolve"} {
		found := false
		for _, r := range e.d.postRoutes {
			found = found || r == p
		}
		if !found {
			t.Errorf("%s not registered through handlePost", p)
		}
		if w := e.post(p, url.Values{"report": {"1"}, "kind": {"host"}}, sc, ""); w.Code != http.StatusForbidden {
			t.Errorf("POST %s without csrf = %d, want 403", p, w.Code)
		}
		if w := e.post(p, url.Values{"report": {"1"}, "kind": {"host"}}, sc, csrf+"x"); w.Code != http.StatusForbidden {
			t.Errorf("POST %s with wrong csrf = %d, want 403", p, w.Code)
		}
	}
}

func TestAdminReportFieldsEscaped(t *testing.T) {
	e := newAdminEnv(t)
	sc, _ := e.signedInAs(t, 42, "alice")
	addReport(t, e.testEnv, "https://x.localhost/a", "<script>x</script>")
	addReport(t, e.testEnv, "javascript:alert(1)", "js url")
	addReport(t, e.testEnv, "https://x.localhost/\"><img src=x onerror=alert(1)>", "quote")
	body := e.do("GET", "/admin", nil, sc).Body.String()
	if strings.Contains(body, "<script>x</script>") || !strings.Contains(body, "&lt;script&gt;x&lt;/script&gt;") {
		t.Errorf("details not escaped: %s", body)
	}
	if strings.Contains(body, `href="javascript:`) || strings.Contains(body, "<img src=x") {
		t.Errorf("unsafe URL rendered: %s", body)
	}
	if !strings.Contains(body, "javascript:alert(1)") {
		t.Errorf("non-http URL should still be shown as text")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func abuseRow(u, details string) store.AbuseReport {
	return store.AbuseReport{URL: u, Category: "phishing", Details: details, Email: "r@example.com", IP: "192.0.2.1"}
}

func mkUserFor(t *testing.T, e *testEnv, ghID int64, login string) int64 {
	t.Helper()
	u, _, err := e.st.UpsertGitHubUser(context.Background(), store.User{GitHubID: ghID, Login: login})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
