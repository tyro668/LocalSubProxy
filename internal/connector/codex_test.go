package connector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// The test binary doubles as a small App Server. It never calls a model.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("codex-cli fake")
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "app-server" {
		fakeCodex()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeCodex() {
	mode := os.Getenv("CODEX_HOME")
	scan := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	write := func(v any) { _ = out.Encode(v) }
	for scan.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &msg) != nil {
			continue
		}
		if len(msg.ID) == 0 {
			continue
		}
		result := func(v any) { write(map[string]any{"id": msg.ID, "result": v}) }
		switch msg.Method {
		case "initialize":
			result(map[string]any{})
		case "account/read":
			accountType := "chatgpt"
			if mode == "apikey" {
				accountType = "apiKey"
			}
			result(map[string]any{"account": map[string]string{"type": accountType}, "requiresOpenaiAuth": true})
		case "model/list":
			result(map[string]any{"data": []map[string]any{{"id": "fake-model", "hidden": false}}})
		case "thread/start":
			var p struct {
				Sandbox        string `json:"sandbox"`
				ApprovalPolicy string `json:"approvalPolicy"`
				Cwd            string `json:"cwd"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.Sandbox != "read-only" || p.ApprovalPolicy != "never" || p.Cwd == "" {
				write(map[string]any{"id": msg.ID, "error": map[string]any{"code": -1, "message": "unsafe thread policy"}})
				continue
			}
			if mode == "instructions" {
				result(map[string]any{"thread": map[string]string{"id": "thread_1"}, "modelProvider": "openai", "instructionSources": []string{"/home/user/AGENTS.md"}})
			} else {
				result(map[string]any{"thread": map[string]string{"id": "thread_1"}, "modelProvider": "openai"})
			}
		case "turn/start":
			var p struct {
				SandboxPolicy struct {
					Type string `json:"type"`
				} `json:"sandboxPolicy"`
				ApprovalPolicy string `json:"approvalPolicy"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.SandboxPolicy.Type != "readOnly" || p.ApprovalPolicy != "never" {
				write(map[string]any{"id": msg.ID, "error": map[string]any{"code": -1, "message": "unsafe turn policy"}})
				continue
			}
			result(map[string]any{"turn": map[string]string{"id": "turn_1"}})
			if strings.HasPrefix(mode, "cancel:") || strings.HasPrefix(mode, "cancel-unconfirmed:") {
				marker := strings.TrimPrefix(strings.TrimPrefix(mode, "cancel-unconfirmed:"), "cancel:")
				_ = os.WriteFile(marker+".ready", []byte("ready"), 0600)
				continue
			}
			if mode == "tool" {
				write(map[string]any{"method": "item/started", "params": map[string]any{"turnId": "turn_1", "item": map[string]string{"id": "tool_1", "type": "commandExecution"}}})
				continue
			}
			if mode == "toolcompleted" {
				write(map[string]any{"method": "item/completed", "params": map[string]any{"turnId": "turn_1", "item": map[string]string{"id": "tool_1", "type": "commandExecution"}}})
				continue
			}
			if mode == "empty" {
				write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread_1", "turn": map[string]string{"id": "turn_1", "status": "completed"}}})
				continue
			}
			if mode == "turnitems" {
				write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread_1", "turn": map[string]any{"id": "turn_1", "status": "completed", "items": []map[string]string{{"id": "item_1", "type": "agentMessage", "phase": "final_answer", "text": "hello world"}}}}})
				continue
			}
			if mode == "quota" {
				write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread_1", "turn": map[string]any{"id": "turn_1", "status": "failed", "error": map[string]string{"message": "limit reached", "codexErrorInfo": "usageLimitExceeded"}}}})
				continue
			}
			write(map[string]any{"method": "item/started", "params": map[string]any{"turnId": "turn_1", "item": map[string]string{"id": "item_1", "type": "agentMessage", "phase": "final_answer"}}})
			write(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"turnId": "turn_1", "itemId": "item_1", "delta": "hello "}})
			write(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"turnId": "turn_1", "itemId": "item_1", "delta": "world"}})
			write(map[string]any{"method": "item/completed", "params": map[string]any{"turnId": "turn_1", "item": map[string]string{"id": "item_1", "type": "agentMessage", "phase": "final_answer", "text": "hello world"}}})
			write(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "thread_1", "turnId": "turn_1", "tokenUsage": map[string]any{"last": map[string]any{"inputTokens": 11, "outputTokens": 7, "totalTokens": 18}, "total": map[string]any{"inputTokens": 11, "outputTokens": 7, "totalTokens": 18}}}})
			write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread_1", "turn": map[string]string{"id": "turn_1", "status": "completed"}}})
		case "turn/interrupt":
			if strings.HasPrefix(mode, "cancel:") {
				_ = os.WriteFile(strings.TrimPrefix(mode, "cancel:")+".interrupt", []byte("received"), 0600)
			}
			result(map[string]any{})
			if strings.HasPrefix(mode, "cancel-unconfirmed:") {
				continue
			}
			write(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread_1", "turn": map[string]string{"id": "turn_1", "status": "interrupted"}}})
		default:
			result(map[string]any{})
		}
	}
}

func fakeMessages() []lmbp.Message {
	return []lmbp.Message{{Role: "user", Content: lmbp.Text("say hello")}}
}

func TestCodexProbeAndStreaming(t *testing.T) {
	t.Setenv("CODEX_HOME", "normal")
	x := &Connector{running: map[string]runSlot{}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	models, err := CodexModels(ctx, os.Args[0])
	if err != nil || len(models) != 1 || models[0] != "fake-model" {
		t.Fatalf("model discovery: %v %v", models, err)
	}
	h := x.probeCodexAdapter(ctx, AdapterConfig{ID: "codex-local", Type: "codex_app_server", CodexBin: os.Args[0], Models: []string{"fake-model"}, Enabled: true})
	if h.Health != "ready" || h.AuthState != "ready" || len(h.Models) != 1 || h.InferenceLocation != "provider_cloud" {
		t.Fatalf("probe: %#v", h)
	}
	var answer string
	if _, err := runCodex(ctx, "fake-model", fakeMessages(), os.Args[0], func(delta string) error { answer += delta; return nil }); err != nil {
		t.Fatal(err)
	}
	if answer != "hello world" {
		t.Fatalf("answer %q", answer)
	}
}

func TestCodexFinalAnswerFromTurnSnapshot(t *testing.T) {
	t.Setenv("CODEX_HOME", "turnitems")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var answer string
	if _, err := runCodex(ctx, "fake-model", fakeMessages(), os.Args[0], func(delta string) error { answer += delta; return nil }); err != nil {
		t.Fatal(err)
	}
	if answer != "hello world" {
		t.Fatalf("answer %q", answer)
	}
}

func TestCodexRejectsUnsupportedExecution(t *testing.T) {
	for _, mode := range []string{"apikey", "tool", "toolcompleted", "instructions", "empty", "quota"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CODEX_HOME", mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := runCodex(ctx, "fake-model", fakeMessages(), os.Args[0], func(string) error { return nil })
			var known knownFailure
			if !errors.As(err, &known) {
				t.Fatalf("expected known failure, got %v", err)
			}
			if mode == "apikey" && !strings.Contains(err.Error(), "AUTH_REQUIRED") {
				t.Fatal(err)
			}
			if (mode == "tool" || mode == "toolcompleted" || mode == "instructions") && !strings.Contains(err.Error(), "POLICY_DENIED") {
				t.Fatal(err)
			}
			if mode == "empty" && !strings.Contains(err.Error(), "PROTOCOL_MISMATCH") {
				t.Fatal(err)
			}
			if mode == "quota" && !strings.Contains(err.Error(), "QUOTA_EXHAUSTED") {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexCancellationSendsInterrupt(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cancel")
	t.Setenv("CODEX_HOME", "cancel:"+marker)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runCodex(ctx, "fake-model", fakeMessages(), os.Args[0], func(string) error { return nil })
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker + ".ready"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker + ".ready"); err != nil {
		cancel()
		t.Fatal("turn did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, errCodexInterrupted) {
			t.Fatalf("cancel result %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation hung")
	}
	if _, err := os.Stat(marker + ".interrupt"); err != nil {
		t.Fatal("turn/interrupt was not sent")
	}
}

func TestCodexCancellationWithoutCompletionIsUncertain(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cancel")
	t.Setenv("CODEX_HOME", "cancel-unconfirmed:"+marker)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runCodex(ctx, "fake-model", fakeMessages(), os.Args[0], func(string) error { return nil })
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker + ".ready"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker + ".ready"); err != nil {
		cancel()
		t.Fatal("turn did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected uncertain cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation hung")
	}
}

func TestCodexTaskThroughExecutor(t *testing.T) {
	t.Setenv("CODEX_HOME", "normal")
	x, err := New(Config{Adapters: []AdapterConfig{{ID: "codex-local", Type: "codex_app_server", CodexBin: os.Args[0], Models: []string{"fake-model"}, Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if caps := x.ModelCapabilities()["codex-local/fake-model"]; len(caps) != 1 || caps[0] != lmbp.ModelCapParams {
		t.Fatalf("codex must be text-only and declare no request parameters: %v", caps)
	}
	var out strings.Builder
	run := lmbp.TaskRun{TaskID: "task_1", Model: "codex-local/fake-model", Messages: fakeMessages()}
	done, fail := x.Execute(ctx, run, func(d lmbp.TaskDelta) error { out.WriteString(d.Text); return nil })
	if fail != nil || done.FinishReason != "stop" || out.String() != "hello world" {
		t.Fatalf("execute: %v %v %q", done, fail, out.String())
	}
	if done.Usage == nil || done.Usage.PromptTokens != 11 || done.Usage.CompletionTokens != 7 {
		t.Fatalf("usage not reported: %+v", done.Usage)
	}
	run.Tools = []lmbp.Tool{{Type: "function", Function: lmbp.FunctionDef{Name: "f"}}}
	if _, fail := x.Execute(ctx, run, func(lmbp.TaskDelta) error { return nil }); fail == nil || fail.Code != lmbp.ErrUnsupported {
		t.Fatalf("tools on codex: %v", fail)
	}
}
