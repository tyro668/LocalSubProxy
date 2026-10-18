package gateway

// Fourth audit A2: a model that declares its parameter support ("params"
// plus one flag per honoured parameter) gets unsupported parameters rejected
// at the gateway with CAPABILITY_UNSUPPORTED, like tools/images/audio.
// Undeclared models (no "params") keep the legacy behaviour (forwarded; the
// device enforces). On /v1/messages max_tokens is mandatory in the API, so a
// model without max_tokens support gets it dropped (documented), not 400.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyro668/avora/internal/lmbp"
)

func postJSON(t *testing.T, url string, body map[string]any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+tenantKey)
	req.Header.Set("X-End-User", "alice")
	req.Header.Set("Content-Type", "application/json")
	resp := doReq(t, req)
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(rb)
}

func TestParamCapabilities(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	chat := func(model string, extra map[string]any) (int, string) {
		b := map[string]any{"model": model, "messages": user("hi there")}
		for k, v := range extra {
			b[k] = v
		}
		return postJSON(t, r.srv.URL+"/v1/chat/completions", b)
	}
	for _, c := range []struct {
		model string
		extra map[string]any
		param string // "" = accepted
	}{
		{"textonly", nil, ""},
		{"textonly", map[string]any{"temperature": 0.5}, "temperature"},
		{"textonly", map[string]any{"top_p": 0.5}, "top_p"},
		{"textonly", map[string]any{"stop": "END"}, "stop"},
		{"textonly", map[string]any{"max_tokens": 10}, "max_tokens"},
		{"temp", map[string]any{"temperature": 0.5}, ""},
		{"temp", map[string]any{"top_p": 0.5}, "top_p"},
		{"echo", map[string]any{"top_p": 0.5, "stop": "END", "max_tokens": 10}, ""}, // undeclared: legacy
	} {
		code, body := chat(c.model, c.extra)
		if c.param == "" {
			if code != 200 {
				t.Errorf("%s %v: %d %s, want 200", c.model, c.extra, code, body)
			}
			continue
		}
		if code != 400 || !strings.Contains(body, lmbp.ErrUnsupported) || !strings.Contains(body, c.param) {
			t.Errorf("%s %v: %d %s, want 400 CAPABILITY_UNSUPPORTED naming %s", c.model, c.extra, code, body, c.param)
		}
	}
	// /v1/messages: max_tokens is mandatory there; dropped for a model that cannot honour it.
	r.exec.runs.Range(func(k, _ any) bool { r.exec.runs.Delete(k); return true })
	code, body := postJSON(t, r.srv.URL+"/v1/messages", map[string]any{"model": "textonly", "max_tokens": 64, "messages": user("hi there")})
	if code != 200 {
		t.Fatalf("/v1/messages on textonly: %d %s", code, body)
	}
	n := 0
	r.exec.runs.Range(func(_, v any) bool {
		n++
		if run := v.(lmbp.TaskRun); run.MaxTokens != 0 {
			t.Errorf("max_tokens %d was forwarded to a model that declares no max_tokens support", run.MaxTokens)
		}
		return true
	})
	if n != 1 {
		t.Errorf("%d runs recorded, want 1", n)
	}
	code, body = postJSON(t, r.srv.URL+"/v1/messages", map[string]any{"model": "textonly", "max_tokens": 64, "top_p": 0.5, "messages": user("hi")})
	if code != 400 || !strings.Contains(body, "top_p") {
		t.Errorf("/v1/messages top_p on textonly: %d %s", code, body)
	}
}
