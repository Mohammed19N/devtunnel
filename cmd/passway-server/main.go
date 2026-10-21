// Command passway-server is the Passway tunnel relay server.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mohammed19n/passway/internal/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stderr, nil)
	stop()
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run starts the server and blocks until ctx is cancelled or a listener fails.
// ready, if non-nil, is called with the bound addresses: the plain HTTP address
// first (if enabled), then the TLS address (if enabled).
func run(ctx context.Context, args []string, stderr io.Writer, ready func(addrs []string)) error {
	fs := flag.NewFlagSet("passway-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	domain := fs.String("domain", "", "base domain, e.g. passway.dev (required)")
	addr := fs.String("addr", ":8080", "plain HTTP listen address (empty disables)")
	tlsAddr := fs.String("tls-addr", "", "TLS listen address")
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (required with --tls-addr)")
	tlsKey := fs.String("tls-key", "", "TLS key file (required with --tls-addr)")
	tokens := fs.String("tokens", "", "comma-separated accepted tokens (empty = open, development only)")
	publicPort := fs.Int("public-port", 0, "port shown in public URLs (0 = the listener's port)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *domain == "" {
		return errors.New("--domain is required")
	}
	useTLS := *tlsAddr != ""
	var cert tls.Certificate
	if useTLS {
		if *tlsCert == "" {
			return errors.New("--tls-cert is required with --tls-addr")
		}
		if *tlsKey == "" {
			return errors.New("--tls-key is required with --tls-addr")
		}
		var err error
		if cert, err = tls.LoadX509KeyPair(*tlsCert, *tlsKey); err != nil {
			return fmt.Errorf("load TLS certificate: %w", err)
		}
	}
	if !useTLS && *addr == "" {
		return errors.New("nothing to listen on: set --addr or --tls-addr")
	}

	var plainLn, tlsLn net.Listener
	var addrs []string
	if *addr != "" {
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", *addr, err)
		}
		plainLn = ln
		addrs = append(addrs, ln.Addr().String())
	}
	if useTLS {
		ln, err := net.Listen("tcp", *tlsAddr)
		if err != nil {
			if plainLn != nil {
				plainLn.Close()
			}
			return fmt.Errorf("listen on %s: %w", *tlsAddr, err)
		}
		tlsLn = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		addrs = append(addrs, ln.Addr().String())
	}

	scheme, pubLn := "http", plainLn
	if useTLS {
		scheme, pubLn = "https", tlsLn
	}
	port := *publicPort
	if port == 0 {
		port = portOf(pubLn.Addr())
	}
	if (scheme == "http" && port == 80) || (scheme == "https" && port == 443) {
		port = 0
	}

	var authenticator server.Authenticator
	if *tokens != "" {
		var list []string
		for _, t := range strings.Split(*tokens, ",") {
			if t = strings.TrimSpace(t); t != "" {
				list = append(list, t)
			}
		}
		authenticator = server.StaticTokens(list)
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	srv := server.New(server.Config{
		Domain:       *domain,
		PublicScheme: scheme,
		PublicPort:   port,
		Auth:         authenticator,
		Logger:       log,
	})

	var servers []*http.Server
	errc := make(chan error, 2)
	serve := func(h http.Handler, ln net.Listener, kind string) {
		hs := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, hs)
		go func() {
			log.Info("listening", "kind", kind, "addr", ln.Addr().String(), "domain", *domain)
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s server: %w", kind, err)
			}
		}()
	}
	if useTLS {
		serve(srv, tlsLn, "https")
		if plainLn != nil {
			serve(redirectHandler(port), plainLn, "http-redirect")
		}
	} else {
		serve(srv, plainLn, "http")
	}
	if ready != nil {
		ready(addrs)
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	srv.CloseTunnels()
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, hs := range servers {
		if err := hs.Shutdown(sctx); err != nil {
			log.Warn("shutdown", "err", err)
			hs.Close()
		}
	}
	return runErr
}

func portOf(a net.Addr) int {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.Port
	}
	return 0
}

// redirectHandler sends every plain-HTTP request to the same URL over HTTPS.
// httpsPort is the public HTTPS port (0 = default 443).
func redirectHandler(httpsPort int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			host = "[" + host + "]" // bare IPv6 literal
		}
		if httpsPort != 0 {
			host += ":" + strconv.Itoa(httpsPort)
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}
