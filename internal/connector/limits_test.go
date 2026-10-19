package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/lmbp"
)

// Backends that cannot enforce a limit refuse to store it: saving fails and
// a stored scheme with it is not served (it used to be silently ignored).
func TestUnenforceableLimitsAreRejected(t *testing.T) {
	cases := []struct {
		typ      string
		cw, mout int
	}{
		{"codex_app_server", 0, 2048}, {"codex_app_server", 128000, 0},
		{"antigravity_cli", 0, 2048}, {"antigravity_cli", 128000, 0},
		{"grok_build", 128000, 0},
	}
	for _, c := range cases {
		a := AdapterConfig{ID: "s", Type: c.typ, Models: []string{"m"}, ContextWindow: c.cw, MaxOutputTokens: c.mout, Enabled: true}
		if err := validateAdapter(&a); err == nil || !strings.Contains(err.Error(), "cannot enforce") {
			t.Fatalf("%s cw=%d out=%d saved: %v", c.typ, c.cw, c.mout, err)
		}
		b := AdapterConfig{ID: "s", Type: c.typ, Models: []string{"m"}, ContextWindow: c.cw, MaxOutputTokens: c.mout, Enabled: true}
		if err := validateLoadedAdapter(&b); err == nil {
			t.Fatalf("%s: stored limit loaded", c.typ)
		}
	}
	grok := AdapterConfig{ID: "s", Type: "grok_build", Models: []string{"m"}, MaxOutputTokens: 2048, Enabled: true}
	if err := validateLoadedAdapter(&grok); err != nil {
		t.Fatalf("grok max output refused: %v", err)
	}
	x, err := New(Config{Adapters: []AdapterConfig{{ID: "cx", Type: "codex_app_server", Models: []string{"m"}, MaxOutputTokens: 4096, Enabled: true}}})
	if err == nil {
		var views []AdapterView
		views = x.ListAdapters()
		if len(views) != 1 || x.invalid["cx"] == "" {
			t.Fatalf("stored codex max output not flagged invalid: %+v %v", views, x.invalid)
		}
	}
}

type openAIStub struct {
	calls atomic.Int32
	body  map[string]any
}

func (s *openAIStub) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		s.calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		s.body = nil
		_ = json.Unmarshal(b, &s.body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func user(text string) []lmbp.Message { return []lmbp.Message{{Role: "user", Content: lmbp.Text(text)}} }

// The scheme's max output reaches the provider; a smaller request value wins.
func TestHTTPMaxOutputReachesProvider(t *testing.T) {
	var s openAIStub
	srv := s.server(t)
	x := newX(t, AdapterConfig{ID: "oa", Type: "openai_compatible", BaseURL: srv.URL + "/v1", APIKey: "k", Models: []string{"m"}, MaxOutputTokens: 77, SkipModelProbe: true})
	var c collected
	if _, fail := x.Execute(context.Background(), run("oa/m", user("hi"), nil), c.emit); fail != nil {
		t.Fatal(fail)
	}
	if s.body["max_tokens"] != float64(77) {
		t.Fatalf("scheme max output not sent: %v", s.body["max_tokens"])
	}
	r := run("oa/m", user("hi"), nil)
	r.MaxTokens = 50
	if _, fail := x.Execute(context.Background(), r, c.emit); fail != nil || s.body["max_tokens"] != float64(50) {
		t.Fatalf("request max_tokens: %v %v", fail, s.body["max_tokens"])
	}
}

// A scheme context window is checked before execution: an over-budget
// request is refused with BAD_REQUEST and never reaches the provider.
func TestContextBudgetRefusesBeforeExecution(t *testing.T) {
	var s openAIStub
	srv := s.server(t)
	x := newX(t, AdapterConfig{ID: "oa", Type: "openai_compatible", BaseURL: srv.URL + "/v1", APIKey: "k", Models: []string{"m"}, ContextWindow: 1000, MaxOutputTokens: 200, SkipModelProbe: true})
	var c collected
	_, fail := x.Execute(context.Background(), run("oa/m", user(strings.Repeat("word ", 1200)), nil), c.emit) // ≥ 1000 estimated + 200 reserved
	if fail == nil || fail.Code != lmbp.ErrBadRequest || !strings.Contains(fail.Message, "CONTEXT_BUDGET_EXCEEDED") || fail.Retryable {
		t.Fatalf("over budget: %+v", fail)
	}
	if s.calls.Load() != 0 {
		t.Fatal("over-budget request reached the provider")
	}
	if _, fail = x.Execute(context.Background(), run("oa/m", user("短问题"), nil), c.emit); fail != nil || s.calls.Load() != 1 {
		t.Fatalf("in-budget request: %v calls=%d", fail, s.calls.Load())
	}
}

// Ollama: the scheme window becomes num_ctx; with none set the model's
// context length is read from /api/show and enforced, and test_adapter
// reports it as detected with its source.
func TestOllamaContextWindow(t *testing.T) {
	var chats atomic.Int32
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"qwen3:8b"}]}`)
		case "/api/show":
			fmt.Fprint(w, `{"model_info":{"general.architecture":"qwen3","qwen3.context_length":4096}}`)
		case "/api/chat":
			chats.Add(1)
			b, _ := io.ReadAll(r.Body)
			got = nil
			_ = json.Unmarshal(b, &got)
			fmt.Fprintln(w, `{"message":{"content":"ok"},"done":true,"prompt_eval_count":1,"eval_count":1}`)
		}
	}))
	defer srv.Close()
	var c collected
	set := newX(t, AdapterConfig{ID: "ol", Type: "ollama_http", BaseURL: srv.URL, Models: []string{"qwen3:8b"}, ContextWindow: 8192})
	if _, fail := set.Execute(context.Background(), run("ol/qwen3:8b", user("hi"), nil), c.emit); fail != nil {
		t.Fatal(fail)
	}
	if opts, _ := got["options"].(map[string]any); opts["num_ctx"] != float64(8192) {
		t.Fatalf("num_ctx not sent: %v", got["options"])
	}
	auto := newX(t, AdapterConfig{ID: "ol2", Type: "ollama_http", BaseURL: srv.URL, Models: []string{"qwen3:8b"}})
	before := chats.Load()
	_, fail := auto.Execute(context.Background(), run("ol2/qwen3:8b", user(strings.Repeat("abcdef", 5000)), nil), c.emit)
	if fail == nil || !strings.Contains(fail.Message, "4096-token context window (detected via Ollama /api/show qwen3.context_length)") || chats.Load() != before {
		t.Fatalf("detected window not enforced: %+v", fail)
	}
	h, err := auto.TestAdapter(context.Background(), AdapterConfig{ID: "ol2", Type: "ollama_http", BaseURL: srv.URL, Models: []string{"qwen3:8b"}})
	if err != nil {
		t.Fatal(err)
	}
	if cw := h.Limits["context_window"]; cw.State != backend.LimitDetected || cw.Value != 4096 || cw.Source != "Ollama /api/show qwen3.context_length" || !cw.Estimate {
		t.Fatalf("limits: %+v", h.Limits)
	}
	if mo := h.Limits["max_output_tokens"]; mo.State != backend.LimitVendorDefault {
		t.Fatalf("max output state: %+v", mo)
	}
}

func TestConfigLimitStates(t *testing.T) {
	views := map[string]map[string]backend.LimitState{}
	for _, a := range []AdapterConfig{
		{ID: "a", Type: "anthropic_compatible", Models: []string{"m"}},
		{ID: "o", Type: "openai_compatible", Models: []string{"m"}, MaxOutputTokens: 900, ContextWindow: 64000},
		{ID: "c", Type: "codex_app_server", Models: []string{"m"}},
	} {
		views[a.ID] = configLimits(a)
	}
	want := map[string][2]string{"a": {backend.LimitAvoraDefault, backend.LimitUnknown}, "o": {backend.LimitUser, backend.LimitUser}, "c": {backend.LimitUnsupported, backend.LimitUnsupported}}
	for id, w := range want {
		if views[id]["max_output_tokens"].State != w[0] || views[id]["context_window"].State != w[1] {
			t.Fatalf("%s: %+v", id, views[id])
		}
	}
	if views["a"]["max_output_tokens"].Value != 1024 || views["o"]["max_output_tokens"].Value != 900 {
		t.Fatalf("values: %+v", views)
	}
}
