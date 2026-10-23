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
}

func (b *banSet) banned(acc Account, label string) bool {
	if b == nil {
		return false
	}
	if label != "" && b.hosts[strings.ToLower(label)] {
		return true
	}
	return acc.Login != "" && b.users[strings.ToLower(acc.Login)]
}

// pollBans refreshes the cached ban set and closes the live sessions of
// banned users and hosts. On error the previous cache stays.
func (s *Server) pollBans(ctx context.Context) {
	users, hosts, err := s.cfg.Directory.BannedLists(ctx)
	if err != nil {
		s.log.Warn("ban poll failed", "err", err)
		return
	}
	set := &banSet{users: make(map[string]bool, len(users)), hosts: make(map[string]bool, len(hosts))}
	for _, u := range users {
		set.users[strings.ToLower(u)] = true
	}
	for _, h := range hosts {
		set.hosts[strings.ToLower(h)] = true
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
