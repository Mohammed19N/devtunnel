package server

// Plan describes the limits of an account tier. Zero on any limit means
// unlimited.
type Plan struct {
	Name          string
	MaxTunnels    int   // concurrent tunnels
	MaxSubdomains int   // reserved subdomains
	RateLimit     int   // requests per minute per tunnel
	Bandwidth     int64 // bytes per month
	Warning       bool  // show the browser interstitial

	MaxCustomDomains int  // custom domains; unlike the limits above, 0 means none
	Branding         bool // edge pages show the "Served by Passway" footer
}

// The plan table. Keep every number here.
var (
	PlanFree = Plan{Name: "free", MaxTunnels: 1, MaxSubdomains: 1, RateLimit: 60, Bandwidth: 1 << 30, Warning: true, Branding: true}
	PlanPro  = Plan{Name: "pro", MaxTunnels: 5, MaxSubdomains: 5, RateLimit: 600, Bandwidth: 50 << 30, MaxCustomDomains: 2}
	// PlanUnlimited is for self-hosted and static-token accounts; its warning
	// follows the server's --browser-warning flag.
	PlanUnlimited = Plan{Name: "unlimited"}
)

// PlanByName returns the named plan; unknown names map to PlanFree.
func PlanByName(name string) Plan {
	switch name {
	case PlanPro.Name:
		return PlanPro
	case PlanUnlimited.Name:
		return PlanUnlimited
	}
	return PlanFree
}
