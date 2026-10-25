package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/mohammed19n/passway/internal/server/store"
)

const adminUsage = `usage: passway-server admin --db PATH [--domain D] <command>
  ban user <login> [--reason R]    ban host <label|custom-domain> [--reason R]
  unban user <login>               unban host <label|custom-domain>
  set-plan <login> free|pro        whois <label>
  admit <login>                    backup <dest>
  token create <name>              token list        token revoke <id>`

// runAdmin runs one admin command against the SQLite database directly and
// prints a single result line to out.
func runAdmin(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("passway-server admin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	db := fs.String("db", "", "SQLite database path (required)")
	domain := fs.String("domain", os.Getenv("PASSWAY_DOMAIN"), "base domain, to catch tunnel hostnames passed to ban host (default $PASSWAY_DOMAIN)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(out, adminUsage)
			return err
		}
		return usageError{err}
	}
	if *db == "" {
		return usageError{errors.New("admin: --db is required")}
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageError{errors.New(adminUsage)}
	}
	// The database is opened by a.ready, after a command has checked its
	// arguments, so a usage error never touches (or creates) the file.
	// Only "token create" may create it, so a fresh self-hosted install can
	// make its first token before the server ever ran.
	creating := len(rest) >= 2 && rest[0] == "token" && rest[1] == "create"
	a := &admin{ctx: ctx, out: out, path: *db, create: creating,
		domain: strings.TrimSuffix(strings.ToLower(strings.TrimSpace(*domain)), ".")}
	defer a.close()
	cmd, rest := rest[0], rest[1:]
	switch cmd {
	case "ban":
		return a.ban(rest)
	case "unban":
		return a.unban(rest)
	case "set-plan":
		return a.setPlan(rest)
	case "admit":
		if len(rest) != 1 {
			return usageError{errors.New("usage: admin admit <login>")}
		}
		return a.admit(rest[0])
	case "whois":
		if len(rest) != 1 {
			return usageError{errors.New("usage: admin whois <label>")}
		}
		return a.whois(rest[0])
	case "backup":
		if len(rest) != 1 {
			return usageError{errors.New("usage: admin backup <dest>")}
		}
		return a.backup(rest[0])
	case "token":
		return a.token(rest)
	}
	return usageError{fmt.Errorf("unknown admin command %q\n%s", cmd, adminUsage)}
}

type admin struct {
	ctx    context.Context
	st     *store.Store // nil until ready
	out    io.Writer
	path   string
	create bool   // ready may create the database
	domain string // the base domain, lowercased; "" = unknown
}

// ready opens the database. Commands call it once their arguments are valid.
func (a *admin) ready() error {
	if a.st != nil {
		return nil
	}
	open := store.OpenExisting
	if a.create {
		open = store.Open
	}
	st, err := open(a.path)
	if err != nil {
		return err
	}
	a.st = st
	return nil
}

func (a *admin) close() {
	if a.st != nil {
		a.st.Close()
	}
}

// target splits "<kind> <value> [--reason R]".
func target(name string, args []string) (kind, value, reason string, err error) {
	if len(args) < 2 || (args[0] != "user" && args[0] != "host") {
		return "", "", "", usageError{fmt.Errorf("usage: admin %s user|host <value>", name)}
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	r := fs.String("reason", "", "reason")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
		return "", "", "", usageError{fmt.Errorf("usage: admin %s user|host <value> [--reason R]", name)}
	}
	return args[0], args[1], *r, nil
}

// checkHost refuses a host ban value that can never match: the edge looks
// tunnels on <label>.<domain> up by their label, so a ban on the full
// hostname would never apply. Custom domains keep their full hostname.
func (a *admin) checkHost(name, value string) error {
	if a.domain == "" {
		return nil
	}
	v := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	if v == a.domain {
		return usageError{fmt.Errorf("admin %s host: %s is the base domain; pass the label of a tunnel (e.g. %s host foo for foo.%s)", name, v, name, a.domain)}
	}
	if label, ok := strings.CutSuffix(v, "."+a.domain); ok {
		return usageError{fmt.Errorf("admin %s host: %s is a tunnel hostname, and tunnels are banned by label; pass the label: %s host %s", name, v, name, label)}
	}
	return nil
}

func (a *admin) user(login string) (store.User, error) {
	if err := a.ready(); err != nil {
		return store.User{}, err
	}
	u, err := a.st.UserByLogin(a.ctx, login)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, fmt.Errorf("no user %q", login)
	}
	return u, err
}

func (a *admin) ban(args []string) error {
	kind, value, reason, err := target("ban", args)
	if err != nil {
		return err
	}
	if kind == "host" {
		if err := a.checkHost("ban", value); err != nil {
			return err
		}
	}
	if err := a.ready(); err != nil {
		return err
	}
	if kind == "host" {
		if err := a.st.AddBan(a.ctx, store.Ban{Kind: "host", Value: value, Reason: reason}); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "banned host %s\n", value)
		return nil
	}
	u, err := a.user(value)
	if err != nil {
		return err
	}
	if err := a.st.BanUser(a.ctx, u, reason); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "banned user @%s\n", u.Login)
	return nil
}

func (a *admin) unban(args []string) error {
	kind, value, _, err := target("unban", args)
	if err != nil {
		return err
	}
	if kind == "host" {
		if err := a.checkHost("unban", value); err != nil {
			return err
		}
	}
	if err := a.ready(); err != nil {
		return err
	}
	if kind == "host" {
		if err := a.st.RemoveBan(a.ctx, "host", value); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "unbanned host %s\n", value)
		return nil
	}
	u, err := a.user(value)
	if err != nil {
		return err
	}
	if err := a.st.UnbanUser(a.ctx, u); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "unbanned user @%s\n", u.Login)
	return nil
}

func (a *admin) setPlan(args []string) error {
	if len(args) != 2 || (args[1] != "free" && args[1] != "pro") {
		return usageError{errors.New("usage: admin set-plan <login> free|pro")}
	}
	u, err := a.user(args[0])
	if err != nil {
		return err
	}
	if err := a.st.SetPlan(a.ctx, u.ID, args[1]); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "@%s is now on the %s plan\n", u.Login, strings.ToUpper(args[1][:1])+args[1][1:])
	return nil
}

// admit takes a user off the waitlist.
func (a *admin) admit(login string) error {
	u, err := a.user(login)
	if err != nil {
		return err
	}
	if err := a.st.SetWaitlisted(a.ctx, u.ID, false); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "admitted @%s\n", u.Login)
	return nil
}

func (a *admin) whois(label string) error {
	if err := a.ready(); err != nil {
		return err
	}
	id, err := a.st.SubdomainOwner(a.ctx, label)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(a.out, "%s -> unclaimed\n", label)
		return nil
	}
	if err != nil {
		return err
	}
	u, err := a.st.UserByID(a.ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s -> @%s (%s, user %d)\n", label, u.Login, u.Plan, u.ID)
	return nil
}

// backup copies the database to dest, which must not exist yet.
func (a *admin) backup(dest string) error {
	if err := a.ready(); err != nil {
		return err
	}
	if err := a.st.Backup(a.ctx, dest); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "backup written to %s\n", dest)
	return nil
}

func (a *admin) token(args []string) error {
	if len(args) == 0 {
		return usageError{errors.New("usage: admin token create <name> | list | revoke <id>")}
	}
	switch args[0] {
	case "create":
		if len(args) != 2 {
			return usageError{errors.New("usage: admin token create <name>")}
		}
		if err := a.ready(); err != nil {
			return err
		}
		plain, t, err := a.st.CreateToken(a.ctx, 0, args[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "token %d %q (shown once): %s\n", t.ID, t.Name, plain)
	case "list":
		if err := a.ready(); err != nil {
			return err
		}
		ts, err := a.st.ListTokens(a.ctx, 0)
		if err != nil {
			return err
		}
		for _, t := range ts {
			state := ""
			if t.RevokedAt != nil {
				state = " revoked"
			}
			fmt.Fprintf(a.out, "%d\t%s\t%s%s\n", t.ID, t.Name, t.Prefix, state)
		}
	case "revoke":
		id, err := strconv.ParseInt(argAt(args, 1), 10, 64)
		if len(args) != 2 || err != nil {
			return usageError{errors.New("usage: admin token revoke <id>")}
		}
		if err := a.ready(); err != nil {
			return err
		}
		if err := a.st.RevokeToken(a.ctx, 0, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no live admin token %d", id)
			}
			return err
		}
		fmt.Fprintf(a.out, "revoked token %d\n", id)
	default:
		return usageError{fmt.Errorf("unknown token command %q", args[0])}
	}
	return nil
}

func argAt(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}
