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
