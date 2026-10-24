package server

import "context"

// Directory is the cloud account directory; a nil Config.Directory means
// self-hosted.
type Directory interface {
	Authenticator
	// Names returns the subdomains acc may use, default first.
	Names(ctx context.Context, acc Account) ([]string, error)
	// Owns reports whether acc owns name.
	Owns(ctx context.Context, acc Account, name string) (bool, error)
	// OwnsDomain reports whether host is a verified custom domain of acc that
	// its plan allows (Plan.MaxCustomDomains, oldest verified first).
	OwnsDomain(ctx context.Context, acc Account, host string) (bool, error)
	// Banned reports whether the account or host label is banned.
	Banned(ctx context.Context, acc Account, label string) (bool, error)
	// AddUsage records bytes for acc this month; Usage returns this month's total.
	AddUsage(ctx context.Context, acc Account, bytes int64) error
	Usage(ctx context.Context, acc Account) (int64, error)
	PlanUsage(ctx context.Context, plan string) (int64, error)
	// BannedLists lists bans for the poller: user logins and host labels.
	BannedLists(ctx context.Context) (users, hosts []string, err error)
	// RevokedTokens returns the IDs among ids whose tokens are revoked.
	RevokedTokens(ctx context.Context, ids []int64) ([]int64, error)
}
