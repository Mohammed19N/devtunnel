package server

import (
	"context"
	"slices"
	"strings"
	"time"
)

const banPollEvery = 15 * time.Second

// banSet is the cached ban lists; entries are lowercase.
type banSet struct {
	users, hosts map[string]bool
	sticky       map[string]string // host label -> user-ban key (login or id:<n>) whose ban closed it
}

func (b *banSet) banned(acc Account, label string) bool {
	if b == nil {
		return false
	}
	if label != "" {
		l := strings.ToLower(label)
		if b.hosts[l] {
			return true
		}
		if _, ok := b.sticky[l]; ok {
			return true
		}
	}
	return b.userKey(acc) != ""
}

// userKey returns the user-ban entry that matches acc (its login, else
// "id:<id>", so a GitHub rename doesn't lift a ban), or "".
func (b *banSet) userKey(acc Account) string {
	if acc.Login != "" {
		if k := strings.ToLower(acc.Login); b.users[k] {
			return k
		}
	}
	if acc.ID != "" {
		if k := "id:" + strings.ToLower(acc.ID); b.users[k] {
			return k
		}
	}
	return ""
}

// pollBans refreshes the cached ban set and closes the live sessions of
// banned users and hosts. On error the previous cache stays. Polls (the
// ticker and PollBansNow) apply their results one at a time under pollMu,
// which is not held across the database read; a poll that a later-started
// poll has already overtaken drops its stale result.
func (s *Server) pollBans(ctx context.Context) {
	s.pollMu.Lock()
	s.pollStarted++
	gen := s.pollStarted
	s.pollMu.Unlock()

	users, hosts, err := s.cfg.Directory.BannedLists(ctx)
	if err != nil {
		s.log.Warn("ban poll failed", "err", err)
		return
	}
	// Tokens revoked by another process (the admin CLI): one query per poll.
	var live []int64
	for _, t := range s.registry.All() {
		if t.Account.TokenID != 0 {
			live = append(live, t.Account.TokenID)
		}
	}
	var revoked []int64
	if len(live) > 0 {
		slices.Sort(live)
		if revoked, err = s.cfg.Directory.RevokedTokens(ctx, slices.Compact(live)); err != nil {
			s.log.Warn("revoked token poll failed", "err", err)
		}
	}

	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	if gen < s.pollApplied {
		return
	}
	s.pollApplied = gen
	set := &banSet{users: make(map[string]bool, len(users)), hosts: make(map[string]bool, len(hosts)), sticky: map[string]string{}}
	for _, u := range users {
		set.users[strings.ToLower(u)] = true
	}
	for _, h := range hosts {
		set.hosts[strings.ToLower(h)] = true
	}
	// Hosts closed for a user ban keep showing the 403 page while that login
	// stays banned.
	if old := s.bans.Load(); old != nil {
		for h, login := range old.sticky {
			if set.users[login] {
				set.sticky[h] = login
			}
		}
	}
	for _, t := range s.registry.All() {
		if set.banned(t.Account, t.Name) && t.session != nil {
			if k := set.userKey(t.Account); k != "" {
				set.sticky[strings.ToLower(t.Name)] = k
			}
		}
	}
	s.bans.Store(set)
	for _, t := range s.registry.All() {
		if t.session == nil {
			continue
		}
		if set.banned(t.Account, t.Name) {
			s.log.Info("closing banned tunnel", "name", t.Name, "login", t.Account.Login)
			t.session.Close()
		} else if t.Account.TokenID != 0 && slices.Contains(revoked, t.Account.TokenID) {
			s.log.Info("closing tunnel of revoked token", "name", t.Name, "account", t.Account.ID, "token", t.Account.TokenID)
			t.session.Close()
		}
	}
}

func (s *Server) runBanPoller(ctx context.Context) {
	s.pollBans(ctx)
	tk := time.NewTicker(banPollEvery)
	defer tk.Stop()
	for {
		select {
		case <-tk.C:
			s.pollBans(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// PollBansNow refreshes the ban cache and closes banned live tunnels right
// away instead of waiting for the next poll. For tests and admin tooling.
func (s *Server) PollBansNow(ctx context.Context) {
	if s.cfg.Directory != nil {
		s.pollBans(ctx)
	}
}
