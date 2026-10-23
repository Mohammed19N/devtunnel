package server

import (
	"context"
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
// banned users and hosts. On error the previous cache stays.
func (s *Server) pollBans(ctx context.Context) {
	users, hosts, err := s.cfg.Directory.BannedLists(ctx)
	if err != nil {
		s.log.Warn("ban poll failed", "err", err)
		return
	}
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
		if set.banned(t.Account, t.Name) && t.session != nil {
			s.log.Info("closing banned tunnel", "name", t.Name, "login", t.Account.Login)
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
