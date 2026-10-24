package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/mohammed19n/passway/internal/server/store"
)

// acmeOpts configures --tls-mode acme. cloudflareToken comes from
// $PASSWAY_CLOUDFLARE_API_TOKEN only and is never logged.
type acmeOpts struct {
	email, dir, ca  string
	cloudflareToken string
}

// acmeDir is --acme-dir, defaulting to <dir of --db>/certs, else ./certs.
func acmeDir(flagDir, db string) string {
	if flagDir != "" {
		return flagDir
	}
	if db != "" {
		return filepath.Join(filepath.Dir(db), "certs")
	}
	return filepath.Join(".", "certs")
}

// certManager holds the two certmagic configs sharing one cache and storage:
// wildcard manages <domain> and *.<domain> over Cloudflare DNS-01 and never
// issues on demand; onDemand (cloud mode with a store only, else nil) issues
// over HTTP-01/TLS-ALPN for names its DecisionFunc (store.AllowCert) accepts.
// They must be separate: certmagic uses DNS-01 exclusively when it is set, and
// custom domains' DNS is not on our Cloudflare account.
type certManager struct {
	domain             string
	cache              *certmagic.Cache
	wildcard, onDemand *certmagic.Config
	onDemandIssuer     *certmagic.ACMEIssuer
}

// underBase reports whether name is domain or within it (any depth).
func underBase(name, domain string) bool {
	return name == domain || strings.HasSuffix(name, "."+domain)
}

// buildCertManager builds the configs without starting any issuance.
func buildCertManager(domain string, o acmeOpts, st *store.Store, log *slog.Logger) (*certManager, error) {
	if o.email == "" {
		return nil, errors.New("acme: an account email is required")
	}
	if o.cloudflareToken == "" {
		return nil, errors.New("acme: PASSWAY_CLOUDFLARE_API_TOKEN is required")
	}
	if o.ca == "" {
		o.ca = certmagic.LetsEncryptProductionCA
	}
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	zl := zap.New(slogCore{log: log})
	storage := &certmagic.FileStorage{Path: o.dir}
	cm := &certManager{domain: domain}
	cm.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(c certmagic.Certificate) (*certmagic.Config, error) {
			for _, n := range c.Names {
				n = strings.ToLower(n)
				if underBase(strings.TrimPrefix(n, "*."), domain) {
					return cm.wildcard, nil
				}
			}
			if cm.onDemand != nil {
				return cm.onDemand, nil
			}
			return cm.wildcard, nil
		},
		Logger: zl,
	})

	cm.wildcard = certmagic.New(cm.cache, certmagic.Config{Storage: storage, DefaultServerName: domain, Logger: zl})
	cm.wildcard.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(cm.wildcard, certmagic.ACMEIssuer{
		CA:     o.ca,
		Email:  o.email,
		Agreed: true,
		DNS01Solver: &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
			DNSProvider: &cloudflare.Provider{APIToken: o.cloudflareToken},
			Logger:      zl,
		}},
		Logger: zl,
	})}

	if st != nil {
		cm.onDemand = certmagic.New(cm.cache, certmagic.Config{
			Storage: storage,
			Logger:  zl,
			OnDemand: &certmagic.OnDemandConfig{
				DecisionFunc: func(ctx context.Context, name string) error {
					name = strings.ToLower(strings.TrimSuffix(name, "."))
					if underBase(name, domain) {
						return fmt.Errorf("%s is under the base domain; only the wildcard covers it", name)
					}
					return st.AllowCert(ctx, name)
				},
			},
		})
		cm.onDemandIssuer = certmagic.NewACMEIssuer(cm.onDemand, certmagic.ACMEIssuer{
			CA:     o.ca,
			Email:  o.email,
			Agreed: true,
			Logger: zl,
		})
		cm.onDemand.Issuers = []certmagic.Issuer{cm.onDemandIssuer}
	}
	return cm, nil
}

// getCertificate routes a handshake: names under the base domain only ever
// get the managed wildcard/base certificate; any other name goes to the
// on-demand config, whose DecisionFunc gates issuance. Without on-demand, a
// random SNI is refused outright.
func (cm *certManager) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c, err := cm.route(hello)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (cm *certManager) route(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if name == "" || underBase(name, cm.domain) {
		return cm.wildcard.GetCertificate(hello)
	}
	if cm.onDemand == nil {
		return nil, fmt.Errorf("no certificate for %q", name)
	}
	return cm.onDemand.GetCertificate(hello)
}

// httpChallenge serves ACME HTTP-01 challenges for on-demand domains; any
// other request gets a 404 (see acmeHTTPHandler for the plain listener).
func (cm *certManager) httpChallenge() http.Handler {
	if cm.onDemandIssuer == nil {
		return http.NotFoundHandler()
	}
	return cm.onDemandIssuer.HTTPChallengeHandler(http.NotFoundHandler())
}

// newCertManager starts managing <domain> and *.<domain> (DNS-01 via
// Cloudflare) in the background and returns the TLS config for the TLS
// listener plus the HTTP-01 challenge handler. On-demand certificates for
// verified custom domains are enabled only when st is non-nil (cloud mode).
// Management stops when ctx is done.
func newCertManager(ctx context.Context, domain string, o acmeOpts, st *store.Store, log *slog.Logger) (*tls.Config, http.Handler, error) {
	cm, err := buildCertManager(domain, o, st, log)
	if err != nil {
		return nil, nil, err
	}
	if err := cm.wildcard.ManageAsync(ctx, []string{cm.domain, "*." + cm.domain}); err != nil {
		cm.cache.Stop()
		return nil, nil, fmt.Errorf("acme: manage %s: %w", cm.domain, err)
	}
	go func() { <-ctx.Done(); cm.cache.Stop() }()
	tc := cm.wildcard.TLSConfig() // carries acme-tls/1 for TLS-ALPN challenges
	tc.GetCertificate = cm.getCertificate
	tc.NextProtos = append([]string{"http/1.1"}, tc.NextProtos...)
	tc.MinVersion = tls.VersionTLS12
	return tc, cm.httpChallenge(), nil
}

// acmeHTTPHandler sends ACME HTTP-01 challenge requests to challenge and
// everything else to next (the HTTPS redirect).
func acmeHTTPHandler(challenge, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if certmagic.LooksLikeHTTPChallenge(r) {
			challenge.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// slogCore sends certmagic's zap logs to the server's slog logger.
type slogCore struct {
	log    *slog.Logger
	fields []zapcore.Field
}

func (c slogCore) Enabled(l zapcore.Level) bool {
	return c.log.Enabled(context.Background(), slogLevel(l))
}

func (c slogCore) With(fs []zapcore.Field) zapcore.Core {
	return slogCore{log: c.log, fields: append(append([]zapcore.Field{}, c.fields...), fs...)}
}

func (c slogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c slogCore) Write(e zapcore.Entry, fs []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range c.fields {
		f.AddTo(enc)
	}
	for _, f := range fs {
		f.AddTo(enc)
	}
	keys := make([]string, 0, len(enc.Fields))
	for k := range enc.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := make([]any, 0, 2*len(keys)+2)
	if e.LoggerName != "" {
		attrs = append(attrs, "logger", e.LoggerName)
	}
	for _, k := range keys {
		attrs = append(attrs, k, enc.Fields[k])
	}
	c.log.Log(context.Background(), slogLevel(e.Level), e.Message, attrs...)
	return nil
}

func (c slogCore) Sync() error { return nil }

func slogLevel(l zapcore.Level) slog.Level {
	switch {
	case l <= zapcore.DebugLevel:
		return slog.LevelDebug
	case l == zapcore.InfoLevel:
		return slog.LevelInfo
	case l == zapcore.WarnLevel:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}
