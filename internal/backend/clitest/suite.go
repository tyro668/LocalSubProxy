package clitest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/lmbp"
)

// Harness adapts one CLI backend to the suite.
type Harness struct {
	// Open returns the backend configured to launch the test binary.
	Open func(t *testing.T) backend.Backend
	// Model is the model ID the fake accepts.
	Model string
	// NotApplicable lists scenarios the protocol cannot express, with the
	// reason. They are logged, not run (verify-all fails on any skip).
	NotApplicable map[string]string
	// EnvAllowed reports whether the CLI may receive an environment key.
	EnvAllowed func(key string) bool
}

const secret = "AVORA_CONFORMANCE_SECRET"

// Run executes every scenario against h.
func Run(t *testing.T, h Harness) {
	run := func(t *testing.T, name string, timeout time.Duration, d Directive, emit func(lmbp.TaskDelta) error) (*lmbp.TaskDone, error, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var text strings.Builder
		if emit == nil {
			emit = func(d lmbp.TaskDelta) error { text.WriteString(d.Text); return nil }
		}
		done, err := h.Open(t).Execute(ctx, lmbp.TaskRun{Model: h.Model, Messages: Messages(d)}, &backend.Emitter{Emit: emit})
		return done, err, text.String()
	}
	code := func(err error) string {
		if b := backend.ErrorBody(err); b != nil {
			return b.Code
		}
		return ""
	}
	seen := map[string]bool{}
	keys := map[string]string{"environment_and_workspace": "report", "cancel_kills_tree": "cancel", "timeout_kills_tree": "timeout", "tool_call_aborts": "tool"}
	scenario := func(name string, f func(*testing.T)) {
		key := name
		if k, ok := keys[name]; ok {
			key = k
		}
		if why, ok := h.NotApplicable[key]; ok {
			seen[key] = true
			t.Logf("%s: not applicable: %s", name, why)
			return
		}
		t.Run(name, f)
	}
	defer func() {
		for key := range h.NotApplicable {
			if !seen[key] {
				t.Errorf("NotApplicable names unknown scenario %q", key)
			}
		}
	}()
	scenario("stream", func(t *testing.T) {
		done, err, text := run(t, "stream", 20*time.Second, Directive{Scenario: "stream"}, nil)
		if err != nil || text != "hello world" || done.FinishReason != "stop" {
			t.Fatalf("done=%+v err=%v text=%q", done, err, text)
		}
		if done.Usage == nil || done.Usage.PromptTokens != 6 || done.Usage.CompletionTokens != 4 {
			t.Fatalf("usage %+v", done.Usage)
		}
	})
	scenario("length", func(t *testing.T) {
		done, err, _ := run(t, "length", 20*time.Second, Directive{Scenario: "length"}, nil)
		if err != nil || done.FinishReason != "length" {
			t.Fatalf("done=%+v err=%v", done, err)
		}
	})
	scenario("cancelled", func(t *testing.T) {
		_, err, _ := run(t, "cancelled", 20*time.Second, Directive{Scenario: "cancelled"}, nil)
		if code(err) != lmbp.ErrCancelled {
			t.Fatalf("err=%v", err)
		}
	})
	scenario("environment_and_workspace", func(t *testing.T) {
		t.Setenv(secret, "must-not-leak")
		t.Setenv("OPENAI_API_KEY", "sk-must-not-leak")
		report := filepath.Join(t.TempDir(), "report.json")
		_, err, text := run(t, "report", 20*time.Second, Directive{Scenario: "report", Report: report}, nil)
		if err != nil || text != "reported" {
			t.Fatalf("err=%v text=%q", err, text)
		}
		var r Report
		b, err := os.ReadFile(report)
		if err != nil || json.Unmarshal(b, &r) != nil {
			t.Fatalf("report: %v", err)
		}
		for _, kv := range r.Env {
			k, v, _ := strings.Cut(kv, "=")
			if strings.Contains(v, "must-not-leak") || !h.EnvAllowed(k) {
				t.Errorf("environment leak: %s", k)
			}
		}
		self, _ := os.Getwd()
		if r.Cwd == "" || r.Cwd == self || !strings.Contains(filepath.Base(r.Cwd), "avora-") {
			t.Errorf("cwd %q is not a private workspace", r.Cwd)
		}
		for _, name := range r.Entries {
			if name == "report.json" || strings.HasSuffix(name, ".go") {
				t.Errorf("workspace exposes %s", name)
			}
		}
		if _, err := os.Stat(r.Cwd); !os.IsNotExist(err) {
			t.Errorf("workspace %s not deleted: %v", r.Cwd, err)
		}
	})
	tree := func(t *testing.T, name string, timeout time.Duration, cancelOnText bool, want string) {
		beat := filepath.Join(t.TempDir(), "beat")
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		emit := func(lmbp.TaskDelta) error {
			if cancelOnText {
				cancel()
			}
			return nil
		}
		start := time.Now()
		_, err := h.Open(t).Execute(ctx, lmbp.TaskRun{Model: h.Model, Messages: Messages(Directive{Scenario: map[string]string{"cancel": "hang", "timeout": "hang", "tool": "tool"}[name], Beat: beat})}, &backend.Emitter{Emit: emit})
		if got := code(err); got != want {
			t.Fatalf("code %s (%v), want %s", got, err, want)
		}
		if time.Since(start) > timeout+10*time.Second {
			t.Fatal("process outlived its task")
		}
		requireStopped(t, beat)
	}
	scenario("cancel_kills_tree", func(t *testing.T) { tree(t, "cancel", 20*time.Second, true, lmbp.ErrCancelled) })
	scenario("timeout_kills_tree", func(t *testing.T) { tree(t, "timeout", 3*time.Second, false, lmbp.ErrTimeout) })
	scenario("tool_call_aborts", func(t *testing.T) { tree(t, "tool", 20*time.Second, false, lmbp.ErrForbidden) })
	scenario("emit_failure", func(t *testing.T) {
		sink := errors.New("sink closed")
		_, err, _ := run(t, "emit_failure", 20*time.Second, Directive{Scenario: "stream"}, func(lmbp.TaskDelta) error { return sink })
		if !errors.Is(err, sink) {
			t.Fatalf("err=%v", err)
		}
	})
	for name, want := range map[string]string{"after_end": lmbp.ErrUpstream, "truncated": lmbp.ErrUpstream, "unknown": lmbp.ErrUpstream, "exposed": lmbp.ErrForbidden, "no_manifest": lmbp.ErrForbidden, "exit_failure": lmbp.ErrUpstream} {
		scenario(name, func(t *testing.T) {
			_, err, _ := run(t, name, 20*time.Second, Directive{Scenario: name}, nil)
			if got := code(err); got != want {
				t.Fatalf("code %s (%v), want %s", got, err, want)
			}
		})
	}
}

// requireStopped fails when the heartbeat file still changes.
func requireStopped(t *testing.T, beat string) {
	t.Helper()
	read := func() []byte {
		for i := 0; i < 20; i++ {
			if b, err := os.ReadFile(beat); err == nil && len(b) > 0 {
				return b
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}
	first := read()
	if first == nil {
		t.Fatal("grandchild never started")
	}
	time.Sleep(100 * time.Millisecond)
	first = read()
	for i := 0; i < 10; i++ {
		time.Sleep(60 * time.Millisecond)
		if !bytes.Equal(first, read()) {
			t.Fatal("grandchild survived the task")
		}
	}
}
