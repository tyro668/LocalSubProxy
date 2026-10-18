package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Rename compatibility: a SaaS that still serves only the pre-rename
// discovery path keeps working; the new path wins when both exist; a
// non-404 failure on the new path is not masked by the fallback.
func TestDiscoverLegacyPathFallback(t *testing.T) {
	serve := func(paths map[string]int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			code, ok := paths[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if code != 200 {
				w.WriteHeader(code)
				return
			}
			_ = json.NewEncoder(w).Encode(Discovery{GatewayURL: "https://gw.example", Tenant: r.URL.Path, Protocol: "lmbp"})
		}))
	}
	for _, tc := range []struct {
		name   string
		paths  map[string]int
		tenant string
		fail   bool
	}{
		{"legacy only", map[string]int{"/.well-known/localsubproxy": 200}, "/.well-known/localsubproxy", false},
		{"new only", map[string]int{"/.well-known/avora": 200}, "/.well-known/avora", false},
		{"both", map[string]int{"/.well-known/avora": 200, "/.well-known/localsubproxy": 200}, "/.well-known/avora", false},
		{"new errors", map[string]int{"/.well-known/avora": 500, "/.well-known/localsubproxy": 200}, "", true},
		{"neither", map[string]int{}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.paths)
			defer srv.Close()
			d, err := Discover(context.Background(), srv.Client(), srv.URL)
			if tc.fail {
				if err == nil {
					t.Fatalf("want error, got %+v", d)
				}
				return
			}
			if err != nil || d.Tenant != tc.tenant {
				t.Fatalf("Discover = %+v, %v; want tenant %s", d, err, tc.tenant)
			}
		})
	}
}
