package connector

// Fourth audit A2: a per-adapter parameter capability table. Every adapter
// either honours a sampling/stop/max_tokens parameter or rejects it with
// CAPABILITY_UNSUPPORTED (it is never silently ignored), and the table is
// advertised in model_capabilities ("params" marker + one flag per honoured
// parameter).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

func f64(v float64) *float64 { return &v }

func TestParamCapabilityTable(t *testing.T) {
	all := []string{lmbp.ModelCapParams, lmbp.ModelCapTemperature, lmbp.ModelCapTopP, lmbp.ModelCapStop, lmbp.ModelCapMaxTokens}
	want := map[string][]string{
		"openai_compatible":    all,
		"anthropic_compatible": all,
		"ollama_http":          all,
		"codex_app_server":     {lmbp.ModelCapParams},
	}
	for typ, flags := range want {
		caps := capsOf(AdapterConfig{Type: typ})
		for _, f := range all {
			if has(caps, f) != has(flags, f) {
				t.Errorf("%s: flag %q advertised=%v, want %v (caps %v)", typ, f, has(caps, f), has(flags, f), caps)
			}
		}
		// configured capabilities (tools/vision/audio) do not drop the param table
		caps = capsOf(AdapterConfig{Type: typ, Capabilities: []string{lmbp.ModelCapTools}})
		if !has(caps, lmbp.ModelCapParams) {
			t.Errorf("%s: configured capabilities dropped the param table: %v", typ, caps)
		}
	}
}

func TestCodexRejectsUnsupportedParams(t *testing.T) {
	t.Setenv("CODEX_HOME", "normal")
	x := newX(t, AdapterConfig{ID: "cx", Type: "codex_app_server", CodexBin: os.Args[0], Models: []string{"fake-model"}})
	base := func() lmbp.TaskRun { return run("cx/fake-model", fakeMessages(), nil) }
	for name, mut := range map[string]func(*lmbp.TaskRun){
		"temperature": func(r *lmbp.TaskRun) { r.Temperature = f64(0.2) },
		"top_p":       func(r *lmbp.TaskRun) { r.TopP = f64(0.5) },
		"stop":        func(r *lmbp.TaskRun) { r.Stop = []string{"END"} },
		"max_tokens":  func(r *lmbp.TaskRun) { r.MaxTokens = 16 },
	} {
		r := base()
		mut(&r)
		_, fail := x.Execute(context.Background(), r, func(lmbp.TaskDelta) error { return nil })
		if fail == nil || fail.Code != lmbp.ErrUnsupported || !strings.Contains(fail.Message, name) {
			t.Errorf("codex with %s: %+v, want CAPABILITY_UNSUPPORTED naming %s", name, fail, name)
		}
	}
}

func TestAnthropicMapsTopPAndStop(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(typ, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data) }
		ev("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"stop_sequence"},"usage":{"output_tokens":1}}`)
		ev("message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	x := newX(t, AdapterConfig{ID: "an", Type: "anthropic_compatible", BaseURL: srv.URL, APIKey: "k", Models: []string{"claude"}})
	r := run("an/claude", []lmbp.Message{{Role: "user", Content: lmbp.Text("hi")}}, nil)
	r.TopP, r.Stop, r.Temperature = f64(0.9), []string{"END", "STOP"}, f64(0.3)
	done, fail := x.Execute(context.Background(), r, func(lmbp.TaskDelta) error { return nil })
	if fail != nil {
		t.Fatal(fail)
	}
	if got["top_p"] != 0.9 || got["temperature"] != 0.3 {
		t.Errorf("sampling not mapped: top_p=%v temperature=%v", got["top_p"], got["temperature"])
	}
	ss, _ := got["stop_sequences"].([]any)
	if len(ss) != 2 || ss[0] != "END" || ss[1] != "STOP" {
		t.Errorf("stop not mapped to stop_sequences: %v", got["stop_sequences"])
	}
	if done.FinishReason != "stop" {
		t.Errorf("stop_sequence finish reason: %q", done.FinishReason)
	}
}
