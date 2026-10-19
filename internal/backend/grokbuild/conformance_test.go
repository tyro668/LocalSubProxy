package grokbuild

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

// wire speaks Grok headless streaming-json.
type wire struct{}

func (wire) line(w io.Writer, v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "%s\n", b) }
func (x wire) Manifest(w io.Writer) {
	x.line(w, map[string]any{"type": "available_commands", "tools": []string{}, "commands": []string{}})
}
func (x wire) Exposed(w io.Writer) {
	x.line(w, map[string]any{"type": "available_commands", "tools": []string{"read_file"}, "commands": []string{}})
}
func (x wire) Text(w io.Writer, s string) { x.line(w, map[string]any{"type": "text", "data": s}) }
func (x wire) End(w io.Writer, stop string, in, out int) {
	x.line(w, map[string]any{"type": "end", "stopReason": stop, "usage": map[string]int{"input_tokens": in, "output_tokens": out}})
}
func (x wire) ToolCall(w io.Writer) {
	x.line(w, map[string]any{"type": "tool_call", "toolCallId": "call_1", "title": "Read", "kind": "read", "status": "in_progress", "toolName": "read_file", "rawInput": map[string]string{"path": "src/main.rs"}})
}
func (x wire) Unknown(w io.Writer) { x.line(w, map[string]any{"type": "future_event"}) }

func TestConformance(t *testing.T) {
	clitest.Run(t, clitest.Harness{Open: func(t *testing.T) backend.Backend { return fixture(t) }, Model: "m", EnvAllowed: func(k string) bool {
		return cliruntime.EnvAllowed(k) || strings.HasPrefix(k, "GROK_")
	}})
}

func run(t *testing.T, model string) (string, *lmbp.TaskDone, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var text strings.Builder
	done, err := fixture(t).Execute(ctx, lmbp.TaskRun{Model: model, Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hello")}}}, &backend.Emitter{Emit: func(d lmbp.TaskDelta) error { text.WriteString(d.Text); return nil }})
	return text.String(), done, err
}

// The samples are doc-derived, not recorded; see testdata/README.md.
func TestDocumentedSamples(t *testing.T) {
	code := func(err error) string {
		if b := backend.ErrorBody(err); b != nil {
			return b.Code
		}
		return ""
	}
	if _, _, err := run(t, "sample-doc-tool-call"); code(err) != lmbp.ErrForbidden {
		t.Fatalf("documented tool call: %v", err)
	}
	if text, _, err := run(t, "sample-doc-readme"); code(err) != lmbp.ErrForbidden || text != "" {
		t.Fatalf("README sample without a tool inventory: %q %v", text, err)
	}
	text, done, err := run(t, "sample-composed-text")
	if err != nil || text != "Here's a summary" || done.FinishReason != "stop" || done.Usage.PromptTokens != 812 || done.Usage.CompletionTokens != 45 {
		t.Fatalf("composed: %q %+v %v", text, done, err)
	}
	if text, done, err = run(t, "sample-composed-readme-endturn"); err != nil || text != "Here's a summary" || done.FinishReason != "stop" {
		t.Fatalf("EndTurn spelling: %q %+v %v", text, done, err)
	}
}

func TestStopReasonSpellings(t *testing.T) {
	for stop, want := range map[string]string{"EndTurn": "stop", "end_turn": "stop", "MaxTokens": "length", "max_tokens": "length", "Cancelled": lmbp.ErrCancelled, "cancelled": lmbp.ErrCancelled, "Refusal": lmbp.ErrUpstream, "max_turn_requests": lmbp.ErrUpstream} {
		s := newStream(&backend.Emitter{Emit: func(lmbp.TaskDelta) error { return nil }})
		if err := s.consume([]byte(`{"type":"available_commands","tools":[],"commands":[]}`)); err != nil {
			t.Fatal(err)
		}
		err := s.consume([]byte(`{"type":"end","stopReason":"` + stop + `"}`))
		got := ""
		if err != nil {
			got = backend.ErrorBody(err).Code
		} else {
			got = s.done.FinishReason
		}
		if got != want {
			t.Errorf("%s -> %s, want %s", stop, got, want)
		}
	}
}

func TestErrorEventsAreClassified(t *testing.T) {
	for model, want := range map[string]string{"rate-limited": "PROVIDER_RATE_LIMITED", "quota": "QUOTA_EXHAUSTED"} {
		_, _, err := run(t, model)
		b := backend.ErrorBody(err)
		if b == nil || !b.Retryable || !strings.HasPrefix(b.Message, want) {
			t.Errorf("%s: %+v", model, b)
		}
	}
}

func TestManifestRequired(t *testing.T) {
	for _, lines := range [][]string{
		{`{"type":"text","data":"x"}`},
		{`{"type":"end","stopReason":"end_turn"}`},
		{`{"type":"available_commands","commands":[]}`},
		{`{"type":"available_commands","tools":["read_file"],"commands":[]}`},
	} {
		s := newStream(&backend.Emitter{Emit: func(lmbp.TaskDelta) error { return nil }})
		var err error
		for _, l := range lines {
			if err = s.consume([]byte(l)); err != nil {
				break
			}
		}
		if b := backend.ErrorBody(err); b == nil || b.Code != lmbp.ErrForbidden {
			t.Errorf("%v accepted: %v", lines, err)
		}
	}
}
