package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/mohammed19n/passway/internal/client"
	"github.com/mohammed19n/passway/internal/config"
	"github.com/mohammed19n/passway/internal/protocol"
)

func runHTTP(ctx context.Context, args []string, stdout, stderr io.Writer, configPath string) int {
	fs := newFlagSet("http", stderr)
	name := fs.String("name", "", "request a specific tunnel name")
	server := fs.String("server", "", "passway server URL")
	hostHeader := fs.String("host-header", "preserve", "preserve|rewrite")
	insecure := fs.Bool("insecure", false, "skip TLS verification (dev only)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: passway http [--name N] [--server URL] [--host-header preserve|rewrite] [--insecure] <target>")
		return 2
	}
	if *hostHeader != "preserve" && *hostHeader != "rewrite" {
		fmt.Fprintf(stderr, "error: --host-header must be preserve or rewrite, got %q\n", *hostHeader)
		return 2
	}
	target, err := client.ParseTarget(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
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

	var mu sync.Mutex // OnReady/OnRequest run on different goroutines
	err = client.Run(ctx, client.Config{
		Server:        srv,
		Token:         token,
		Name:          *name,
		Target:        target,
		RewriteHost:   *hostHeader == "rewrite",
		Insecure:      *insecure,
		ClientVersion: version,
		Logger:        slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		OnReady: func(r protocol.Ready) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(stdout, "Passway %s                                   (Ctrl+C to quit)\n\nForwarding   %s -> %s\n\n", version, r.URL, target)
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
