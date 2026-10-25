package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBaseDomainRoutesToDashboard(t *testing.T) {
	dash := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	serve := func(s *Server, host, path string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = host
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	with := New(Config{Domain: "passway.test", PublicScheme: "https", Auth: StaticTokens{"t"}, Dashboard: dash})
	for path, want := range map[string]int{
		"/":          http.StatusTeapot,
		"/dashboard": http.StatusTeapot,
		"/healthz":   http.StatusOK,
	} {
		if got := serve(with, "passway.test", path); got != want {
			t.Errorf("with dashboard: %s = %d, want %d", path, got, want)
		}
	}
	if got := serve(with, "passway.test", "/_tunnel"); got == http.StatusTeapot {
		t.Error("/_tunnel routed to the dashboard")
	}
	if got := serve(with, "app.passway.test", "/"); got == http.StatusTeapot {
		t.Error("tunnel subdomain routed to the dashboard")
	}
	without := New(Config{Domain: "passway.test", PublicScheme: "https", Auth: StaticTokens{"t"}})
	if got := serve(without, "passway.test", "/dashboard"); got != http.StatusNotFound {
		t.Errorf("without dashboard: /dashboard = %d, want 404", got)
	}
}

func TestStats(t *testing.T) {
	s := New(Config{Domain: "x.test"})
	a := &Tunnel{Name: "aaa", SessionID: "1", Account: Account{ID: "1", Plan: PlanFree}}
	b := &Tunnel{Name: "bbb", SessionID: "2", Account: Account{ID: "2", Plan: PlanPro}}
	for _, tn := range []*Tunnel{a, b} {
		if _, err := s.registry.Claim(ClaimRequest{Name: tn.Name, SessionID: tn.SessionID}, tn); err != nil {
			t.Fatal(err)
		}
	}
	a.requests.Add(5)
	b.requests.Add(7)
	got := s.Stats()
	if got.Tunnels != 2 || got.Requests != 12 || got.ByPlan["free"] != 1 || got.ByPlan["pro"] != 1 {
		t.Fatalf("Stats = %+v", got)
	}
}
