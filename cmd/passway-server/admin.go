package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/mohammed19n/passway/internal/server/store"
)

const adminUsage = `usage: passway-server admin --db PATH <command>
  ban user <login> [--reason R]    ban host <label> [--reason R]
  unban user <login>               unban host <label>
  set-plan <login> free|pro        whois <label>
  token create <name>              token list        token revoke <id>`

// runAdmin runs one admin command against the SQLite database directly and
// prints a single result line to out.
func runAdmin(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("passway-server admin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	db := fs.String("db", "", "SQLite database path (required)")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if *db == "" {
		return usageError{errors.New("admin: --db is required")}
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageError{errors.New(adminUsage)}
	}
	st, err := store.Open(*db)
	if err != nil {
		return fmt.Errorf("open %s: %w", *db, err)
	}
	defer st.Close()
	a := admin{ctx: ctx, st: st, out: out}
	cmd, rest := rest[0], rest[1:]
	switch cmd {
	case "ban":
		return a.ban(rest)
	case "unban":
		return a.unban(rest)
	case "set-plan":
		return a.setPlan(rest)
	case "whois":
		if len(rest) != 1 {
			return usageError{errors.New("usage: admin whois <label>")}
		}
		return a.whois(rest[0])
	case "token":
		return a.token(rest)
	}
	return usageError{fmt.Errorf("unknown admin command %q\n%s", cmd, adminUsage)}
}

type admin struct {
	ctx context.Context
	st  *store.Store
	out io.Writer
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

func (a admin) user(login string) (store.User, error) {
	u, err := a.st.UserByLogin(a.ctx, login)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, fmt.Errorf("no user %q", login)
	}
	return u, err
}

func (a admin) ban(args []string) error {
	kind, value, reason, err := target("ban", args)
	if err != nil {
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

func (a admin) unban(args []string) error {
	kind, value, _, err := target("unban", args)
	if err != nil {
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

func (a admin) setPlan(args []string) error {
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
	fmt.Fprintf(a.out, "@%s is now on the %s plan\n", u.Login, args[1])
	return nil
}

func (a admin) whois(label string) error {
	id, err := a.st.SubdomainOwner(a.ctx, label)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(a.out, "%s → unclaimed\n", label)
		return nil
	}
	if err != nil {
		return err
	}
	u, err := a.st.UserByID(a.ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s → @%s (%s, user %d)\n", label, u.Login, u.Plan, u.ID)
	return nil
}

func (a admin) token(args []string) error {
	if len(args) == 0 {
		return usageError{errors.New("usage: admin token create <name> | list | revoke <id>")}
	}
	switch args[0] {
	case "create":
		if len(args) != 2 {
			return usageError{errors.New("usage: admin token create <name>")}
		}
		plain, t, err := a.st.CreateToken(a.ctx, 0, args[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.out, "token %d %q (shown once): %s\n", t.ID, t.Name, plain)
	case "list":
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
