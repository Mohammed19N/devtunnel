package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/mohammed19n/passway/internal/client"
	"github.com/mohammed19n/passway/internal/config"
	"github.com/mohammed19n/passway/internal/inspector"
	"github.com/mohammed19n/passway/internal/protocol"
)

const defaultInspectAddr = "127.0.0.1:4040"

// checkLoopbackAddr rejects inspector addresses that are not loopback.
func checkLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --inspect-addr %q: %w", addr, err)
	}
	if host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("inspector must listen on a loopback address, got %q", addr)
		}
	}
	return nil
}

// listenInspector binds addr; if addr is the default and in use, it tries
// 127.0.0.1:4041..4049. A non-loopback host is rejected before binding.
func listenInspector(addr string) (net.Listener, error) {
	if err := checkLoopbackAddr(addr); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err == nil || addr != defaultInspectAddr || !errors.Is(err, syscall.EADDRINUSE) {
		return ln, err
	}
	for port := 4041; port <= 4049; port++ {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			return ln, nil
		}
	}
	return nil, fmt.Errorf("inspector: %s and 127.0.0.1:4041-4049 are all in use (use --inspect-addr)", addr)
}

func runHTTP(ctx context.Context, args []string, stdout, stderr io.Writer, configPath string) int {
	fs := newFlagSet("http", stderr)
	name := fs.String("name", "", "request a specific tunnel name")
	server := fs.String("server", "", "passway server URL")
	hostHeader := fs.String("host-header", "preserve", "preserve|rewrite")
	insecure := fs.Bool("insecure", false, "skip TLS verification (dev only)")
	inspect := fs.Bool("inspect", true, "run the local request inspector")
	inspectAddr := fs.String("inspect-addr", defaultInspectAddr, "inspector listen address (loopback only)")
	// Flags may appear before and/or after the target.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: passway http [--name N] [--server URL] [--host-header preserve|rewrite] [--insecure] [--inspect=false] [--inspect-addr ADDR] <target>")
		return 2
	}
	if *hostHeader != "preserve" && *hostHeader != "rewrite" {
		fmt.Fprintf(stderr, "error: --host-header must be preserve or rewrite, got %q\n", *hostHeader)
		return 2
	}
	target, err := client.ParseTarget(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if *inspect {
		if err := checkLoopbackAddr(*inspectAddr); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 2
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	srv := *server
	if srv == "" {
		srv = cfg.Server
	}
	if srv == "" {
		srv = defaultServer
	}
	token := os.Getenv("PASSWAY_TOKEN")
	if token == "" {
		token = cfg.Token
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rewrite := *hostHeader == "rewrite"
	proxy := client.NewProxy(target, rewrite, log)
	var insp *inspector.Inspector
	var handler http.Handler
	inspectorURL := ""
	if *inspect {
		ln, err := listenInspector(*inspectAddr)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		inspectorURL = "http://" + ln.Addr().String()
		insp = inspector.New(inspector.DefaultCapacity, proxy)
		insp.SetTarget(target.String())
		handler = insp.Handler()
		// No WriteTimeout: it would kill the SSE stream.
		hs := &http.Server{Handler: insp.UI(), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = hs.Serve(ln) }()
		defer func() {
			sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer scancel()
			if hs.Shutdown(sctx) != nil {
				_ = hs.Close() // SSE streams keep Shutdown waiting
			}
		}()
	}

	var mu sync.Mutex // OnReady/OnRequest run on different goroutines
	err = client.Run(ctx, client.Config{
		Handler:       handler,
		Server:        srv,
		Token:         token,
		Name:          *name,
		Target:        target,
		RewriteHost:   rewrite,
		Insecure:      *insecure,
		ClientVersion: version,
		Logger:        log,
		OnReady: func(r protocol.Ready) {
			if insp != nil {
				insp.SetPublicURL(r.URL)
			}
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(stdout, "Passway %s                                   (Ctrl+C to quit)\n\nForwarding   %s -> %s\n", version, r.URL, target)
			if inspectorURL != "" {
				fmt.Fprintf(stdout, "Inspector    %s\n", inspectorURL)
			}
			fmt.Fprintln(stdout)
		},
		OnRequest: func(l client.RequestLog) {
			path := l.Path
			if len(path) > 40 {
				path = path[:40]
			}
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(stdout, "%-7s %-40s %3d %6s\n", l.Method, path, l.Status, l.Duration.Round(time.Millisecond))
		},
	})
	if err != nil {
		var perr *protocol.Error
		if errors.As(err, &perr) || ctx.Err() == nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	}
	return 0
}
