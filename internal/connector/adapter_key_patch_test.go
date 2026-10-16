package connector_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/connector"
)

type authRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *authRecorder) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, q.Header.Get("Authorization")+"|"+q.Header.Get("x-api-key"))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (r *authRecorder) leaked(secret string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.seen {
		if strings.Contains(h, secret) {
			return true
		}
	}
	return false
}

// Review8 P1-b: a partial patch that moves an adapter to another endpoint or
// provider type without an api_key must not carry the stored key along, and
// the post-save health probe must never send that key to the new host.
func TestPatchToNewEndpointDoesNotInheritKey(t *testing.T) {
	const secret = "sk-OLD-SECRET"
	cases := []struct{ name, patch string }{
		{"base_url", `{"id":"lm","base_url":"%NEW%"}`},
		{"base_url_null_key", `{"id":"lm","base_url":"%NEW%","api_key":null}`},
		{"type", `{"id":"lm","type":"anthropic_compatible","base_url":"%NEW%"}`},
		{"type_only", `{"id":"lm","type":"anthropic_compatible"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var oldRec, newRec authRecorder
			oldSrv, newSrv := oldRec.server(t), newRec.server(t)
			c := newAdapterCore(t, filepath.Join(t.TempDir(), "adapters.enc"))
			defer c.Shutdown()
			cfg := lmCfg
			cfg.BaseURL, cfg.APIKey, cfg.SkipModelProbe = oldSrv.URL+"/v1", secret, false
			if _, err := c.CreateAdapter(cfg); err != nil {
				t.Fatal(err)
			}
			target := newSrv.URL + "/v1"
			if tc.name == "type_only" {
				target = oldSrv.URL + "/v1" // same host, other provider protocol
			}
			patch := strings.ReplaceAll(tc.patch, "%NEW%", target)
			_, err := c.PatchAdapter("lm", json.RawMessage(patch))
			if err == nil {
				time.Sleep(500 * time.Millisecond) // let the post-save probe run
				t.Fatalf("patch without key to a new endpoint was accepted; old key sent to new host: %v", newRec.leaked(secret) || oldRec.leaked(secret) && tc.name == "type_only")
			}
			if !strings.Contains(err.Error(), "API key required") {
				t.Fatalf("patch without key to a new endpoint: err=%v (want key required)", err)
			}
			if v := storedOf(t, c, "lm"); v.BaseURL != cfg.BaseURL || v.Type != cfg.Type {
				t.Fatalf("rejected patch was stored: %+v", v)
			}
			// a probe of the (still unchanged) stored adapter plus any
			// background refresh: the new host never sees the old key
			time.Sleep(300 * time.Millisecond)
			if tc.name != "type_only" && newRec.leaked(secret) {
				t.Fatal("old API key was sent to the new endpoint")
			}
			// explicit new key: accepted, and only the new key goes out
			if tc.name != "type_only" {
				p := strings.TrimSuffix(patch, "}") + `,"api_key":"sk-NEW"}`
				p = strings.Replace(p, `"api_key":null,`, "", 1)
				if _, err := c.PatchAdapter("lm", json.RawMessage(p)); err != nil {
					t.Fatal(err)
				}
				time.Sleep(500 * time.Millisecond)
				if newRec.leaked(secret) {
					t.Fatal("old key reached new endpoint after re-entry")
				}
			}
		})
	}
}

// The same endpoint keeps the stored key for edits that omit it.
func TestPatchSameEndpointKeepsKey(t *testing.T) {
	c := newAdapterCore(t, filepath.Join(t.TempDir(), "adapters.enc"))
	defer c.Shutdown()
	if _, err := c.CreateAdapter(lmCfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{`{"id":"lm","models":["m2"]}`, `{"id":"lm","name":"x","api_key":""}`, `{"id":"lm","base_url":"http://127.0.0.1:9"}`} {
		if _, err := c.PatchAdapter("lm", json.RawMessage(p)); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !storedOf(t, c, "lm").HasAPIKey {
			t.Fatalf("%s dropped the key", p)
		}
	}
	_ = connector.AdapterConfig{}
}
