// Command passway exposes a local web server through a Passway tunnel.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mohammed19n/passway/internal/config"
)

var version = "dev"                         // set with -ldflags at release
var defaultServer = "http://localhost:8080" // set with -ldflags once the domain exists

const usage = `usage: passway <command> [arguments]

Commands:
  http [--name N | --domain D] [--server URL] [--host-header preserve|rewrite] [--insecure] [--inspect=false] [--inspect-addr ADDR] <target>
  login <token>
  version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	path, err := config.DefaultPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		stop()
		os.Exit(1)
	}
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, path)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, configPath string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "passway %s\n", version)
		return 0
	case "login":
		return runLogin(args[1:], stdout, stderr, configPath)
	case "http":
		return runHTTP(ctx, args[1:], stdout, stderr, configPath)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("passway "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func runLogin(args []string, stdout, stderr io.Writer, configPath string) int {
	fs := newFlagSet("login", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: passway login <token>")
		return 2
	}
	c, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	c.Token = fs.Arg(0)
	if err := config.Save(configPath, c); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Token saved to %s\n", configPath)
	return 0
}
