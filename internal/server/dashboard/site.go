package dashboard

import (
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/mohammed19n/passway"
	"github.com/mohammed19n/passway/internal/server"
)

var (
	//go:embed static/app.css
	appCSS []byte
	//go:embed static/inspector.png
	inspectorPNG []byte
	//go:embed static/og.png
	ogPNG []byte
	//go:embed static/favicon.svg
	faviconSVG []byte
)

// staticFile is an embedded asset served under /static/ with a content hash.
type staticFile struct {
	body        []byte
	contentType string
	version     string // first 8 hex digits of the body's sha256
}

func newStatic(body []byte, contentType string) staticFile {
	sum := sha256.Sum256(body)
	return staticFile{body: body, contentType: contentType, version: hex.EncodeToString(sum[:])[:8]}
}

var staticFiles = map[string]staticFile{
	"app.css":       newStatic(appCSS, "text/css; charset=utf-8"),
	"inspector.png": newStatic(inspectorPNG, "image/png"),
}

// assetURL is the template func "asset": the versioned URL of a static file.
func assetURL(name string) string {
	f, ok := staticFiles[name]
	if !ok {
		panic("dashboard: unknown asset " + name)
	}
	return "/static/" + name + "?v=" + f.version
}

const (
	cachePublic    = "public, max-age=300"
	cachePrivate   = "private, no-store"
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheDay       = "public, max-age=86400"
)

// sitePages is every public HTML route in sitemap order.
var sitePages = []string{"/", "/pricing", "/docs", "/docs/webhooks", "/terms", "/privacy", "/aup", "/abuse"}

func (d *Dashboard) siteRoutes() {
	m := d.mux
	m.HandleFunc("GET /{$}", d.sitePage("home", seoHome, d.homeLD))
	m.HandleFunc("GET /pricing", d.sitePage("pricing", seoPricing, nil))
	m.HandleFunc("GET /docs", d.sitePage("docs", seoDocs, d.articleLD(seoDocs)))
	m.HandleFunc("GET /docs/webhooks", d.sitePage("docs_webhooks", seoWebhooks, d.articleLD(seoWebhooks)))
	m.HandleFunc("GET /terms", d.sitePage("terms", seoTerms, nil))
	m.HandleFunc("GET /privacy", d.sitePage("privacy", seoPrivacy, nil))
	m.HandleFunc("GET /aup", d.sitePage("aup", seoAUP, nil))
	m.HandleFunc("GET /abuse", d.sitePage("abuse", seoAbuse, nil))
	m.HandleFunc("POST /abuse", d.handleAbuse)
	m.HandleFunc("GET /sitemap.xml", d.handleSitemap)
	m.HandleFunc("GET /robots.txt", d.handleRobots)
	m.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		serveBytes(w, "text/plain; charset=utf-8", cachePublic, passway.InstallScript)
	})
	m.HandleFunc("GET /og.png", func(w http.ResponseWriter, r *http.Request) {
		serveBytes(w, "image/png", cacheDay, ogPNG)
	})
	m.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		serveBytes(w, "image/svg+xml", cacheDay, faviconSVG)
	})
	m.HandleFunc("GET /static/{name}", func(w http.ResponseWriter, r *http.Request) {
		f, ok := staticFiles[r.PathValue("name")]
		if !ok {
			d.fail(w, http.StatusNotFound, "There's nothing here.")
			return
		}
		cc := "no-store" // an old or missing version must not stick in caches
		if r.URL.Query().Get("v") == f.version {
			cc = cacheImmutable
		}
		serveBytes(w, f.contentType, cc, f.body)
	})
}

func serveBytes(w http.ResponseWriter, contentType, cacheControl string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	_, _ = w.Write(body)
}

// siteCache sets the caching headers of a public page: shared caches may keep
// it unless the request carries the session cookie, whose page differs
// ("Open dashboard") and may set cookies.
func (d *Dashboard) siteCache(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Cookie")
	for _, c := range r.Cookies() {
		if c.Name == d.sessionCookie {
			w.Header().Set("Cache-Control", cachePrivate)
			return
		}
	}
	w.Header().Set("Cache-Control", cachePublic)
}

// siteView resolves the visitor's session and returns the view every public
// page starts from. ok is false when an error page was written instead.
func (d *Dashboard) siteView(w http.ResponseWriter, r *http.Request, meta seo) (view, bool) {
	s, err := d.currentSession(w, r)
	if err != nil {
		d.internalError(w, "session", err)
		return view{}, false
	}
	d.siteCache(w, r)
	w.Header().Set("Content-Security-Policy", siteCSP)
	operator := d.cfg.LegalOperator
	if operator == "" {
		operator = "the operator of this service"
	}
	jurisdiction := d.cfg.LegalJurisdiction
	if jurisdiction == "" {
		jurisdiction = "the place where the operator of this service is established"
	}
	v := view{
		Title: meta.Title, SEO: &meta, BaseURL: d.cfg.BaseURL, Host: strings.TrimPrefix(strings.TrimPrefix(d.cfg.BaseURL, "https://"), "http://"),
		InstallCmd: "curl -fsSL " + d.cfg.BaseURL + "/install.sh | sh",
		Free:       newPlanCard(server.PlanFree, ""),
		Pro:        newPlanCard(server.PlanPro, "Coming soon"),
		Operator:   operator, Jurisdiction: jurisdiction, AbuseEmail: d.cfg.AbuseEmail,
		Billing: d.cfg.Billing != nil, EdgeHost: d.cfg.EdgeHost,
	}
	v.Free.Price = "$0"
	if v.Billing {
		v.Pro.Price = d.cfg.ProPrice.Label()
		if cur := strings.ToUpper(d.cfg.ProPrice.Currency); cur != "" && cur != "USD" {
			v.Free.Price = "0 " + cur
		}
	}
	if s != nil {
		v.User = &s.User
	}
	return v, true
}

// sitePage serves a public page; ld, when set, builds its JSON-LD.
func (d *Dashboard) sitePage(name string, meta seo, ld func() any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := meta
		if ld != nil {
			m.JSONLD = mustJSONLD(ld())
		}
		v, ok := d.siteView(w, r, m)
		if !ok {
			return
		}
		if name == "abuse" {
			v.Sent = r.URL.Query().Get("sent") == "1"
			v.Abuse.Category = "phishing"
		}
		d.render(w, http.StatusOK, name, v)
	}
}

// planCard is one plan's limits as the pricing pages show them.
type planCard struct {
	Price                                string
	Tunnels, Subdomains, Bandwidth, Rate string
	CustomDomains                        string
	Warning, Branding                    bool
}

func newPlanCard(p server.Plan, price string) planCard {
	c := planCard{
		Price: price, Tunnels: limitText(p.MaxTunnels), Subdomains: limitText(p.MaxSubdomains),
		Bandwidth: "unlimited", Rate: limitText(p.RateLimit), CustomDomains: "Not included",
		Warning: p.Warning, Branding: p.Branding,
	}
	if p.Bandwidth > 0 {
		c.Bandwidth = formatBytes(p.Bandwidth)
	}
	if p.MaxCustomDomains > 0 {
		c.CustomDomains = strconv.Itoa(p.MaxCustomDomains)
	}
	return c
}

// gzipWriter compresses html, css, xml and plain-text responses for clients
// that accept gzip. The decision is made when the header is written.
type gzipWriter struct {
	http.ResponseWriter
	accept  bool
	decided bool
	zw      *gzip.Writer
}

func newGzipWriter(w http.ResponseWriter, r *http.Request) *gzipWriter {
	return &gzipWriter{ResponseWriter: w, accept: acceptsGzip(r.Header.Get("Accept-Encoding"))}
}

// acceptsGzip reports whether an Accept-Encoding value allows gzip.
func acceptsGzip(ae string) bool {
	for _, part := range strings.Split(ae, ",") {
		coding, params, _ := strings.Cut(part, ";")
		if strings.TrimSpace(strings.ToLower(coding)) != "gzip" {
			continue
		}
		q := strings.ReplaceAll(strings.ToLower(params), " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
}

func compressible(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	switch strings.TrimSpace(mt) {
	case "text/html", "text/css", "text/plain", "application/xml", "text/xml":
		return true
	}
	return false
}

func (g *gzipWriter) WriteHeader(code int) {
	if !g.decided {
		g.decided = true
		h := g.Header()
		if compressible(h.Get("Content-Type")) && h.Get("Content-Encoding") == "" {
			h.Add("Vary", "Accept-Encoding")
			if g.accept && code >= 200 && code != http.StatusNoContent && code != http.StatusNotModified {
				h.Del("Content-Length")
				h.Set("Content-Encoding", "gzip")
				g.zw = gzip.NewWriter(g.ResponseWriter)
			}
		}
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.decided {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.zw != nil {
		return g.zw.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) close() {
	if g.zw != nil {
		_ = g.zw.Close()
	}
}
