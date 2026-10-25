package dashboard

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// publicPages is every public HTML route.
var publicPages = []string{"/", "/pricing", "/docs", "/docs/webhooks", "/terms", "/privacy", "/aup", "/abuse"}

// htmlDoc is a parsed page with helpers for the SEO checks.
type htmlDoc struct{ root *html.Node }

func parseHTML(t *testing.T, body string) htmlDoc {
	t.Helper()
	n, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return htmlDoc{n}
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// all returns every element named tag.
func (d htmlDoc) all(tag string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(d.root)
	return out
}

func text(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// meta returns the content of every <meta> whose key attribute ("name" or
// "property") is val.
func (d htmlDoc) meta(key, val string) []string {
	var out []string
	for _, m := range d.all("meta") {
		if v, _ := attr(m, key); v == val {
			c, _ := attr(m, "content")
			out = append(out, c)
		}
	}
	return out
}

func (d htmlDoc) links(rel string) []string {
	var out []string
	for _, l := range d.all("link") {
		if v, _ := attr(l, "rel"); v == rel {
			h, _ := attr(l, "href")
			out = append(out, h)
		}
	}
	return out
}

// jsonLD returns the bodies of every application/ld+json script.
func (d htmlDoc) jsonLD() []string {
	var out []string
	for _, s := range d.all("script") {
		if v, _ := attr(s, "type"); v == "application/ld+json" {
			out = append(out, text(s))
		}
	}
	return out
}

func one(t *testing.T, what string, vals []string) string {
	t.Helper()
	if len(vals) != 1 {
		t.Errorf("%s: %d values %q, want exactly 1", what, len(vals), vals)
		return ""
	}
	return vals[0]
}

func TestPublicPagesSEO(t *testing.T) {
	e := newTestEnv(t, nil)
	titles := map[string]string{}
	descs := map[string]string{}
	for _, p := range publicPages {
		w := e.do("GET", p, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: code %d", p, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", p, ct)
		}
		body := w.Body.String()
		if strings.Contains(strings.ToLower(body), "noindex") || strings.Contains(strings.ToLower(w.Header().Get("X-Robots-Tag")), "noindex") {
			t.Errorf("%s: noindex", p)
		}
		doc := parseHTML(t, body)
		htmls := doc.all("html")
		if lang, _ := attr(htmls[0], "lang"); lang != "en" {
			t.Errorf("%s: html lang = %q", p, lang)
		}
		if !strings.Contains(body, `<html lang="en">`) {
			t.Errorf("%s: no literal <html lang=\"en\">", p)
		}

		var tl []string
		for _, n := range doc.all("title") {
			tl = append(tl, text(n))
		}
		title := one(t, p+" title", tl)
		if n := utf8.RuneCountInString(title); n == 0 || n > 60 {
			t.Errorf("%s: title %q is %d chars, want 1-60", p, title, n)
		}
		if other, dup := titles[title]; dup {
			t.Errorf("%s: title %q also used by %s", p, title, other)
		}
		titles[title] = p

		desc := one(t, p+" description", doc.meta("name", "description"))
		if n := utf8.RuneCountInString(desc); n < 50 || n > 160 {
			t.Errorf("%s: description %q is %d chars, want 50-160", p, desc, n)
		}
		if other, dup := descs[desc]; dup {
			t.Errorf("%s: description also used by %s", p, other)
		}
		descs[desc] = p

		canonical := testBaseURL + p
		if got := one(t, p+" canonical", doc.links("canonical")); got != canonical {
			t.Errorf("%s: canonical %q, want %q", p, got, canonical)
		}
		if got := one(t, p+" og:title", doc.meta("property", "og:title")); got == "" {
			t.Errorf("%s: empty og:title", p)
		}
		if got := one(t, p+" og:description", doc.meta("property", "og:description")); got == "" {
			t.Errorf("%s: empty og:description", p)
		}
		if got := one(t, p+" og:url", doc.meta("property", "og:url")); got != canonical {
			t.Errorf("%s: og:url %q, want %q", p, got, canonical)
		}
		if got := one(t, p+" og:image", doc.meta("property", "og:image")); got != testBaseURL+"/og.png" {
			t.Errorf("%s: og:image %q", p, got)
		}
		if got := one(t, p+" og:type", doc.meta("property", "og:type")); got == "" {
			t.Errorf("%s: empty og:type", p)
		}
		if got := one(t, p+" twitter:card", doc.meta("name", "twitter:card")); got != "summary_large_image" {
			t.Errorf("%s: twitter:card %q", p, got)
		}
		if n := len(doc.all("h1")); n != 1 {
			t.Errorf("%s: %d <h1>, want 1", p, n)
		}
		if len(doc.meta("name", "viewport")) != 1 {
			t.Errorf("%s: no viewport meta", p)
		}
		for _, img := range doc.all("img") {
			if alt, ok := attr(img, "alt"); !ok || alt == "" {
				t.Errorf("%s: <img> without alt", p)
			}
		}
		for _, ld := range doc.jsonLD() {
			var v map[string]any
			if err := json.Unmarshal([]byte(ld), &v); err != nil {
				t.Errorf("%s: JSON-LD: %v\n%s", p, err, ld)
			}
		}
		// The footer links every site page and GitHub.
		for _, href := range []string{`href="/docs"`, `href="/pricing"`, `href="/terms"`, `href="/privacy"`,
			`href="/aup"`, `href="/abuse"`, `href="https://github.com/mohammed19n/passway"`} {
			if !strings.Contains(body, href) {
				t.Errorf("%s: footer missing %s", p, href)
			}
		}
	}
}

func TestDashboardPagesNoindex(t *testing.T) {
	e := newTestEnv(t, func(c *Config) { c.DevLogin = true })
	sc, _ := e.signedIn(t)
	pages := []struct {
		path string
		sc   *http.Cookie
	}{
		{"/dashboard", sc}, {"/dashboard/tokens", sc}, {"/dashboard/subdomains", sc}, {"/dashboard/domains", sc},
		{"/auth/dev", nil}, {"/nope", nil}, {"/docs/nope", nil},
	}
	for _, pg := range pages {
		w := e.do("GET", pg.path, nil, pg.sc)
		doc := parseHTML(t, w.Body.String())
		if got := doc.meta("name", "robots"); len(got) != 1 || got[0] != "noindex" {
			t.Errorf("%s (%d): robots meta = %q, want noindex", pg.path, w.Code, got)
		}
		if c := doc.links("canonical"); len(c) != 0 {
			t.Errorf("%s: canonical %q on a noindex page", pg.path, c)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", pg.path, cc)
		}
		// The shared footer still links the public site.
		for _, href := range []string{`href="/docs"`, `href="/pricing"`, `href="/terms"`, `href="/privacy"`, `href="/aup"`, `href="/abuse"`} {
			if !strings.Contains(w.Body.String(), href) {
				t.Errorf("%s: footer missing %s", pg.path, href)
			}
		}
	}
}

func TestJSONLDValid(t *testing.T) {
	ldOf := func(t *testing.T, e *testEnv, path string) map[string]any {
		t.Helper()
		lds := parseHTML(t, e.do("GET", path, nil).Body.String()).jsonLD()
		if len(lds) != 1 {
			t.Fatalf("%s: %d JSON-LD scripts, want 1", path, len(lds))
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(lds[0]), &v); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, lds[0])
		}
		if v["@context"] != "https://schema.org" {
			t.Errorf("%s: @context %v", path, v["@context"])
		}
		return v
	}
	offers := func(v map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		list, _ := v["offers"].([]any)
		for _, o := range list {
			m, _ := o.(map[string]any)
			name, _ := m["name"].(string)
			out[name] = m
		}
		return out
	}

	t.Run("billing off", func(t *testing.T) {
		e := newTestEnv(t, nil)
		v := ldOf(t, e, "/")
		for k, want := range map[string]string{
			"@type": "SoftwareApplication", "name": "Passway", "applicationCategory": "DeveloperApplication",
			"operatingSystem": "Linux, macOS, Windows", "license": "https://github.com/mohammed19n/passway",
		} {
			if v[k] != want {
				t.Errorf("%s = %v, want %q", k, v[k], want)
			}
		}
		o := offers(v)
		if len(o) != 1 || o["Free"] == nil || o["Free"]["price"] != "0" || o["Free"]["@type"] != "Offer" {
			t.Errorf("offers = %v, want only Free at 0", v["offers"])
		}
		for _, p := range []string{"/docs", "/docs/webhooks"} {
			d := ldOf(t, e, p)
			if d["@type"] != "TechArticle" || d["headline"] == "" || d["headline"] == nil {
				t.Errorf("%s: JSON-LD = %v, want a TechArticle with a headline", p, d)
			}
		}
	})
	t.Run("billing on", func(t *testing.T) {
		e, _ := billingEnv(t, nil)
		o := offers(ldOf(t, e, "/"))
		pro := o["Pro"]
		if len(o) != 2 || o["Free"] == nil || pro == nil {
			t.Fatalf("offers = %v, want Free and Pro", o)
		}
		if pro["price"] != "7.00" || pro["priceCurrency"] != "USD" {
			t.Errorf("Pro offer = %v, want 7.00 USD", pro)
		}
	})
}

func TestSitemapListsPublicRoutes(t *testing.T) {
	e := newTestEnv(t, nil)
	w := e.do("GET", "/sitemap.xml", nil)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/xml") {
		t.Fatalf("sitemap = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	var set struct {
		XMLName xml.Name `xml:"urlset"`
		NS      string   `xml:"xmlns,attr"`
		URLs    []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal(w.Body.Bytes(), &set); err != nil {
		t.Fatalf("%v\n%s", err, w.Body)
	}
	if set.NS != "http://www.sitemaps.org/schemas/sitemap/0.9" {
		t.Errorf("xmlns = %q", set.NS)
	}
	var got []string
	for _, u := range set.URLs {
		got = append(got, u.Loc)
	}
	var want []string
	for _, p := range publicPages {
		want = append(want, testBaseURL+p)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("sitemap locs = %q, want %q", got, want)
	}
}

func TestRobotsDisallows(t *testing.T) {
	e := newTestEnv(t, nil)
	w := e.do("GET", "/robots.txt", nil)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("robots = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	lines := map[string]bool{}
	for _, l := range strings.Split(w.Body.String(), "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	for _, want := range []string{"User-agent: *", "Disallow: /dashboard", "Disallow: /login", "Disallow: /logout",
		"Disallow: /auth/", "Disallow: /billing/", "Disallow: /stripe/", "Disallow: /waitlist", "Disallow: /admin",
		"Sitemap: " + testBaseURL + "/sitemap.xml"} {
		if !lines[want] {
			t.Errorf("robots.txt missing %q:\n%s", want, w.Body)
		}
	}
	for _, p := range publicPages {
		if lines["Disallow: "+p] {
			t.Errorf("robots.txt disallows public page %s", p)
		}
	}
}
