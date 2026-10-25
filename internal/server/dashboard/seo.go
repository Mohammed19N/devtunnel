package dashboard

import (
	"encoding/json"
	"encoding/xml"
	"html/template"
	"net/http"
	"strings"
)

// seo is a public page's search and social metadata. JSONLD is built from Go
// structs with json.Marshal (which escapes <, > and &), never from user text.
type seo struct {
	Title, Description, Path string
	JSONLD                   template.JS
}

var (
	seoHome = seo{Path: "/",
		Title:       "Passway: public URLs for localhost with a webhook inspector",
		Description: "Expose a local app at a public HTTPS URL with one command. Inspect, replay and edit webhooks in a local inspector. Open source and self-hostable."}
	seoPricing = seo{Path: "/pricing",
		Title:       "Pricing - Passway",
		Description: "Compare the Passway Free and Pro plans: concurrent tunnels, reserved subdomains, monthly bandwidth, request rate and custom domains."}
	seoDocs = seo{Path: "/docs",
		Title:       "Docs: quickstart and CLI reference - Passway",
		Description: "Install the Passway CLI, open your first tunnel, use the request inspector and look up every command and flag. Links to the self-hosting guide."}
	seoWebhooks = seo{Path: "/docs/webhooks",
		Title:       "Debug Stripe, GitHub and Shopify webhooks locally - Passway",
		Description: "Receive Stripe, GitHub and Shopify webhooks on localhost through a Passway tunnel, inspect each delivery and replay it without triggering a new event."}
	seoTerms = seo{Path: "/terms",
		Title:       "Terms of service - Passway",
		Description: "The terms that apply when you use the hosted Passway tunnel service: accounts, acceptable use, plans and billing, liability and termination."}
	seoPrivacy = seo{Path: "/privacy",
		Title:       "Privacy policy - Passway",
		Description: "What data the hosted Passway service keeps about you, why, and for how long. Inspector data stays on your own machine and is never uploaded."}
	seoAUP = seo{Path: "/aup",
		Title:       "Acceptable use policy - Passway",
		Description: "What you may not do with Passway tunnels: no phishing, malware, illegal content or spam. How we act on reports and suspend accounts."}
	seoAbuse = seo{Path: "/abuse",
		Title:       "Report abuse - Passway",
		Description: "Report a Passway tunnel URL that hosts phishing, malware, spam or other content that breaks the acceptable use policy."}
)

func mustJSONLD(v any) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		panic("dashboard: JSON-LD: " + err.Error())
	}
	return template.JS(b)
}

type ldThing struct {
	Type string `json:"@type"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

type ldOffer struct {
	Type          string `json:"@type"`
	Name          string `json:"name"`
	Price         string `json:"price"`
	PriceCurrency string `json:"priceCurrency"`
	URL           string `json:"url"`
}

type ldApp struct {
	Context             string    `json:"@context"`
	Type                string    `json:"@type"`
	Name                string    `json:"name"`
	Description         string    `json:"description"`
	URL                 string    `json:"url"`
	Image               string    `json:"image"`
	ApplicationCategory string    `json:"applicationCategory"`
	OperatingSystem     string    `json:"operatingSystem"`
	License             string    `json:"license"`
	CodeRepository      string    `json:"codeRepository,omitempty"`
	Offers              []ldOffer `json:"offers"`
}

type ldArticle struct {
	Context          string  `json:"@context"`
	Type             string  `json:"@type"`
	Headline         string  `json:"headline"`
	Description      string  `json:"description"`
	URL              string  `json:"url"`
	MainEntityOfPage string  `json:"mainEntityOfPage"`
	Image            string  `json:"image"`
	InLanguage       string  `json:"inLanguage"`
	Author           ldThing `json:"author"`
	Publisher        ldThing `json:"publisher"`
}

// homeLD is the landing page's SoftwareApplication.
func (d *Dashboard) homeLD() any {
	currency := "USD"
	if d.cfg.Billing != nil && d.cfg.ProPrice.Currency != "" {
		currency = strings.ToUpper(d.cfg.ProPrice.Currency)
	}
	app := ldApp{
		Context: "https://schema.org", Type: "SoftwareApplication", Name: "Passway",
		Description: seoHome.Description, URL: d.cfg.BaseURL + "/", Image: d.cfg.BaseURL + "/og.png",
		ApplicationCategory: "DeveloperApplication", OperatingSystem: "Linux, macOS, Windows",
		License: d.cfg.RepoURL,
		Offers:  []ldOffer{{Type: "Offer", Name: "Free", Price: "0", PriceCurrency: currency, URL: d.cfg.BaseURL + "/pricing"}},
	}
	if d.cfg.Billing != nil {
		app.Offers = append(app.Offers, ldOffer{Type: "Offer", Name: "Pro", Price: d.cfg.ProPrice.Decimal(),
			PriceCurrency: currency, URL: d.cfg.BaseURL + "/pricing"})
	}
	return app
}

// articleLD returns the TechArticle builder for a docs page.
func (d *Dashboard) articleLD(meta seo) func() any {
	return func() any {
		org := ldThing{Type: "Organization", Name: "Passway", URL: d.cfg.BaseURL + "/"}
		return ldArticle{
			Context: "https://schema.org", Type: "TechArticle",
			Headline: strings.TrimSuffix(meta.Title, " - Passway"), Description: meta.Description,
			URL: d.cfg.BaseURL + meta.Path, MainEntityOfPage: d.cfg.BaseURL + meta.Path,
			Image: d.cfg.BaseURL + "/og.png", InLanguage: "en", Author: org, Publisher: org,
		}
	}
}

func (d *Dashboard) handleSitemap(w http.ResponseWriter, r *http.Request) {
	type loc struct {
		Loc string `xml:"loc"`
	}
	set := struct {
		XMLName xml.Name `xml:"urlset"`
		NS      string   `xml:"xmlns,attr"`
		URLs    []loc    `xml:"url"`
	}{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for _, p := range sitePages {
		set.URLs = append(set.URLs, loc{d.cfg.BaseURL + p})
	}
	b, err := xml.MarshalIndent(set, "", "  ")
	if err != nil {
		d.internalError(w, "sitemap", err)
		return
	}
	serveBytes(w, "application/xml; charset=utf-8", cachePublic, append([]byte(xml.Header), append(b, '\n')...))
}

// robotsDisallow lists the private prefixes crawlers should skip.
var robotsDisallow = []string{"/dashboard", "/login", "/logout", "/auth/", "/billing/", "/stripe/", "/waitlist", "/admin"}

func (d *Dashboard) handleRobots(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString("User-agent: *\nAllow: /\n")
	for _, p := range robotsDisallow {
		b.WriteString("Disallow: " + p + "\n")
	}
	b.WriteString("\nSitemap: " + d.cfg.BaseURL + "/sitemap.xml\n")
	serveBytes(w, "text/plain; charset=utf-8", cachePublic, []byte(b.String()))
}
