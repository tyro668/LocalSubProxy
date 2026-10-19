package antigravity

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/cliruntime"
	"github.com/tyro668/avora/internal/backend/clitest"
	"github.com/tyro668/avora/internal/lmbp"
)

//go:embed testdata/*.jsonl
var samples embed.FS

// wire speaks agy stream-json after init (the fake sends init itself,
// because agy reads the prompt only after announcing the session).
type wire struct{ text strings.Builder }

func (*wire) line(w io.Writer, v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "%s\n", b) }
func (*wire) Manifest(io.Writer)      {}
func (*wire) Exposed(io.Writer)       {}
func (x *wire) Text(w io.Writer, s string) {
	x.text.WriteString(s)
	x.line(w, map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": "session", "step_index": 1, "state": "ACTIVE", "step_type": "agent_response", "text_delta": s}})
}
func (x *wire) End(w io.Writer, stop string, in, out int) {
	status := map[string]string{"end_turn": "SUCCESS", "cancelled": "CANCELED"}[stop]
	x.line(w, map[string]any{"event": "result", "result": map[string]any{"conversation_id": "session", "status": status, "response": x.text.String(), "usage": map[string]int{"input_tokens": in, "output_tokens": out, "total_tokens": in + out}}})
}
func (x *wire) ToolCall(w io.Writer) {
	x.line(w, map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": "session", "step_index": 1, "state": "DONE", "step_type": "tool", "tool_name": "run_command"}})
}
func (x *wire) Unknown(w io.Writer) { x.line(w, map[string]any{"event": "future_event"}) }

func TestConformance(t *testing.T) {
	clitest.Run(t, clitest.Harness{
		Open:       func(t *testing.T) backend.Backend { return fixture(t) },
		Model:      "m",
		EnvAllowed: cliruntime.EnvAllowed,
		NotApplicable: map[string]string{
			"length":      "agy result has no max-token status",
			"exposed":     "init precedes the prompt; see TestInitIsTheManifest",
			"no_manifest": "init precedes the prompt; see TestInitIsTheManifest",
		},
	})
}

func execute(t *testing.T, model string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := fixture(t).Execute(ctx, lmbp.TaskRun{Model: model, Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hello")}}}, &backend.Emitter{Emit: func(lmbp.TaskDelta) error { return nil }})
	return err
}

// agy's init names the selected agent; nothing counts before it.
func TestInitIsTheManifest(t *testing.T) {
	for _, model := range []string{"early-step", "wrong-agent"} {
		if b := backend.ErrorBody(execute(t, model)); b == nil || b.Code != lmbp.ErrForbidden {
			t.Errorf("%s: %+v", model, b)
		}
	}
}

func TestResultErrorsAreClassified(t *testing.T) {
	for model, want := range map[string]string{"rate-limited": "PROVIDER_RATE_LIMITED", "quota": "QUOTA_EXHAUSTED"} {
		b := backend.ErrorBody(execute(t, model))
		if b == nil || !b.Retryable || !strings.HasPrefix(b.Message, want) {
			t.Errorf("%s: %+v", model, b)
		}
	}
}

func runSample(t *testing.T, name string) (string, *lmbp.TaskDone, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var text strings.Builder
	done, err := fixture(t).Execute(ctx, lmbp.TaskRun{Model: "sample-" + name, Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hello")}}}, &backend.Emitter{Emit: func(d lmbp.TaskDelta) error { text.WriteString(d.Text); return nil }})
	return text.String(), done, err
}

// The samples are doc-derived, not recorded; see testdata/README.md.
func TestDocumentedSamples(t *testing.T) {
	if _, _, err := runSample(t, "doc-stream"); backend.ErrorBody(err).Code != lmbp.ErrForbidden {
		t.Fatalf("init without the inference agent: %v", err)
	}
	if _, _, err := runSample(t, "composed-tool"); backend.ErrorBody(err).Code != lmbp.ErrForbidden {
		t.Fatalf("documented tool step: %v", err)
	}
	text, done, err := runSample(t, "composed-stream")
	if err != nil || !strings.HasPrefix(text, "Git rebase destructively") || done.FinishReason != "stop" || done.Usage.PromptTokens != 10418 || done.Usage.CompletionTokens != 589 {
		t.Fatalf("stream: %q %+v %v", text, done, err)
	}
	if text, done, err = runSample(t, "composed-chunks"); err != nil || text != "apple\n" || done.Usage.PromptTokens != 30384 {
		t.Fatalf("chunks: %q %+v %v", text, done, err)
	}
}
