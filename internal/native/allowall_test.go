package native

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/avora/mock/fakemodel"
)

// 允许全部模型: the desktop approves a pairing with allow ["*"]. A scheme
// added after the pairing is served to that SaaS without editing the
// connection, through the real gateway → device path.
func TestAllowAllCoversSchemeAddedAfterPairing(t *testing.T) {
	s := newTestSaaS(t, "acme")
	startKernel(t, dir(t))
	defer must(t, map[string]any{"action": "stop"})
	if st := pairOnce(t, s, []string{"*"}); st["state"] != "paired" {
		t.Fatalf("pairing: %v", st)
	}
	cs := conns(t)
	if len(cs) != 1 {
		t.Fatalf("connections %v", cs)
	}
	c := cs[0].(map[string]any)
	if a, _ := c["allow"].([]any); len(a) != 1 || a[0] != "*" {
		t.Fatalf("stored allow %v, want [*]", c["allow"])
	}

	model := httptest.NewServer((&fakemodel.Server{Models: []string{"fresh-1"}}).Handler())
	defer model.Close()
	must(t, map[string]any{"action": "create_adapter", "adapter": map[string]any{
		"id": "later", "type": "openai_compatible", "base_url": model.URL, "api_key": "k",
		"models": []string{"fresh-1"}, "enabled": true,
	}})

	chat := func() (int, string) {
		body := `{"model":"` + c["device_id"].(string) + `/later/fresh-1","messages":[{"role":"user","content":"hi"}]}`
		req, _ := http.NewRequest("POST", s.gw+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.key)
		req.Header.Set("X-End-User", "alice")
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	var code int
	var b string
	for i := 0; i < 100; i++ {
		if code, b = chat(); code == 200 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code != 200 || !strings.Contains(b, "fresh-1") {
		t.Fatalf("new scheme under *: %d %s", code, b)
	}
}
