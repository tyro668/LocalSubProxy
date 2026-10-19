package grokbuild

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

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/clitest"
	"github.com/tyro668/avora/internal/lmbp"
)

func TestMain(m *testing.M) {
	clitest.RunGrandchild()
	if len(os.Args) > 1 && os.Args[1] == "--no-auto-update" {
		fakeCLI()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func fakeCLI() {
	args := os.Args[1:]
	has := func(s string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}
	value := func(s string) string {
		for i, a := range args {
			if a == s && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	if has("stdio") {
		if value("--sandbox") != "read-only" {
			os.Exit(47)
		}
		scanner := bufio.NewScanner(os.Stdin)
		out := json.NewEncoder(os.Stdout)
		for scanner.Scan() {
			var request struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				os.Exit(45)
			}
			var result any = map[string]any{}
			if request.Method == "session/new" {
				result = json.RawMessage(`{"models":{"availableModels":[{"modelId":"m","_meta":{"supportsReasoningEffort":true,"reasoningEffort":"high","reasoningEfforts":[{"value":"low"},{"value":"medium"},{"value":"high","default":true},{"value":"xhigh"}]}},{"modelId":"second","_meta":{"supportsReasoningEffort":false}}]}}`)
			}
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		}
		return
	}
	if has("--help") {
		fmt.Println("--prompt-file --tools --disallowed-tools --agent --verbatim --permission-mode --disable-web-search")
		return
	}
	if has("models") {
		fmt.Println("Available models:\n  * m (default)\n  - second")
		return
	}
	if value("--tools") != "read_file" || value("--disallowed-tools") != "read_file,search_tool,use_tool" || value("--permission-mode") != "dontAsk" || has("--always-approve") || has("--continue") {
		os.Exit(40)
	}
	agent, err := os.ReadFile(value("--agent"))
	if err != nil || !strings.Contains(string(agent), "agentsMd: false") {
		os.Exit(41)
	}
	prompt, err := os.ReadFile(value("--prompt-file"))
	if d, ok := clitest.Find(string(prompt)); ok {
		os.Exit(clitest.Play(d, os.Stdout, wire{}))
	}
	if err != nil || !strings.Contains(string(prompt), "[user]\nhello") {
		os.Exit(42)
	}
	if _, expected, ok := strings.Cut(string(prompt), "expect-effort="); ok {
		want := strings.Fields(expected)[0]
		got := value("--reasoning-effort")
		if got == "" {
			got = "default"
		}
		if got != want {
			os.Exit(46)
		}
	}
	if _, expected, ok := strings.Cut(string(prompt), "expect-max-out="); ok {
		cfg, _ := os.ReadFile(filepath.Join(os.Getenv("GROK_HOME"), "config.toml"))
		want := "[model.\"" + value("--model") + "\"]\nmax_completion_tokens = " + strings.Fields(expected)[0] + "\n"
		if !strings.Contains(string(cfg), want) || !strings.Contains(string(cfg), "[memory]\nenabled = false") {
			os.Exit(49)
		}
	}
	if !insideWorkdir(os.Getenv("GROK_HOME")) || os.Getenv("GROK_AUTH_PATH") == "" || os.Getenv("GROK_CLAUDE_HOOKS_ENABLED") != "0" {
		os.Exit(43)
	}
	model := value("--model")
	if name, ok := strings.CutPrefix(model, "sample-"); ok {
		data, err := samples.ReadFile("testdata/" + name + ".jsonl")
		if err != nil {
			os.Exit(48)
		}
		os.Stdout.Write(data)
		return
	}
	switch model {
	case "rate-limited":
		fmt.Println(`{"type":"error","message":"Rate limit exceeded. Please retry in 30 seconds."}`)
	case "quota":
		fmt.Println(`{"type":"error","message":"You have reached your usage limit for this billing period."}`)
	case "hang":
		time.Sleep(time.Hour)
	case "truncated":
		fmt.Println(`{"type":"text","data":"partial"}`)
	case "exit-failure":
		fmt.Println(`{"type":"end","stopReason":"end_turn"}`)
		os.Exit(2)
	default:
		fmt.Println(`{"type":"available_commands","tools":[],"commands":[]}`)
		fmt.Println(`{"type":"text","data":"hello"}`)
		fmt.Println(`{"type":"text","data":" world"}`)
		fmt.Println(`{"type":"end","stopReason":"end_turn","usage":{"input_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":1,"output_tokens":4}}`)
	}
}
func fixture(t *testing.T) backend.Backend {
	t.Helper()
	home := t.TempDir()
	return Definition().New(backend.Config{Type: "grok_build", Executable: os.Args[0], Models: []string{"m"}, Enabled: true}, backend.Dependencies{UserHomeDir: func() (string, error) { return home, nil }})
}
func TestProcessContract(t *testing.T) {
	a := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if models, err := a.Models(ctx); err != nil || len(models) != 2 {
		t.Fatalf("models: %v %v", models, err)
	}
	var text strings.Builder
	done, err := a.Execute(ctx, lmbp.TaskRun{Model: "m", Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hello")}}}, &backend.Emitter{Emit: func(d lmbp.TaskDelta) error { text.WriteString(d.Text); return nil }})
	if err != nil || done.FinishReason != "stop" || text.String() != "hello world" || done.Usage.PromptTokens != 6 || done.Usage.CompletionTokens != 4 {
		t.Fatalf("result: %v %v %q", done, err, text.String())
	}
}
func TestProcessFailures(t *testing.T) {
	for _, mode := range []string{"truncated", "exit-failure", "hang", "emit-error"} {
		t.Run(mode, func(t *testing.T) {
			a := fixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			sinkErr := errors.New("sink closed")
			_, err := a.Execute(ctx, lmbp.TaskRun{Model: mode, Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hello")}}}, &backend.Emitter{Emit: func(lmbp.TaskDelta) error {
				if mode == "emit-error" {
					return sinkErr
				}
				return nil
			}})
			if err == nil {
				t.Fatal("expected failure")
			}
			if mode == "hang" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if mode == "emit-error" && !errors.Is(err, sinkErr) {
				t.Fatal(err)
			}
		})
	}
}
func TestStreamRejectsActionsAndInvalidTerminal(t *testing.T) {
	for _, line := range []string{`{"type":"tool_call"}`, `{"type":"available_commands","tools":["bash"]}`, `{"type":"end","stopReason":"tool_use"}`, `not json`} {
		s := newStream(&backend.Emitter{Emit: func(lmbp.TaskDelta) error { return nil }})
		if s.consume([]byte(line)) == nil {
			t.Fatalf("accepted %s", line)
		}
	}
	s := newStream(&backend.Emitter{Emit: func(lmbp.TaskDelta) error { return nil }})
	if err := s.consume([]byte(`{"type":"available_commands","tools":[],"commands":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.consume([]byte(`{"type":"end","stopReason":"end_turn"}`)); err != nil {
		t.Fatal(err)
	}
	if s.consume([]byte(`{"type":"end","stopReason":"end_turn"}`)) == nil {
		t.Fatal("accepted duplicate terminal")
	}
}
func TestUnauthenticatedFallbackCatalogIsNotReady(t *testing.T) {
	_, err := parseModels("You are not authenticated.\nAvailable models:\n * grok-4.7 (default)")
	if err == nil || !strings.Contains(err.Error(), "AUTH_REQUIRED") {
		t.Fatal(err)
	}
}
func TestWorkspaceIsRemoved(t *testing.T) {
	a := fixture(t).(*Adapter)
	o, cleanup, err := a.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(o.Dir, ".git", "HEAD")); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err = os.Stat(o.Dir); !os.IsNotExist(err) {
		t.Fatalf("workspace leaked: %v", err)
	}
}

func TestReasoningCatalogAndWireEffort(t *testing.T) {
	a := fixture(t).(*Adapter)
	catalog, err := a.Catalog(context.Background())
	if err != nil || len(catalog) != 2 || !catalog[0].ReasoningKnown || catalog[0].DefaultReasoningEffort != "high" || len(catalog[0].ReasoningEfforts) != 4 || len(catalog[1].ReasoningEfforts) != 0 {
		t.Fatalf("catalog: %+v %v", catalog, err)
	}
	for _, effort := range []string{"", "default", "off", "low", "medium", "high", "xhigh", "none", "max"} {
		t.Run(effort, func(t *testing.T) {
			a := fixture(t).(*Adapter)
			a.config.ReasoningEffort = effort
			want := Definition().Effort(effort)
			if want == "" {
				want = "default"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			_, err := a.Execute(ctx, lmbp.TaskRun{Model: "m", Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hello\nexpect-effort=" + want)}}}, &backend.Emitter{Emit: func(lmbp.TaskDelta) error { return nil }})
			if effort == "none" || effort == "max" {
				if err == nil || !strings.Contains(err.Error(), "CAPABILITY_UNSUPPORTED") {
					t.Fatalf("unsupported accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCatalogDoesNotInventEfforts(t *testing.T) {
	catalog, err := parseCatalog([]byte(`{"models":{"availableModels":[{"modelId":"unknown"},{"modelId":"fixed","_meta":{"supportsReasoningEffort":false,"reasoningEfforts":[{"value":"low"}]}},{"modelId":"optional","_meta":{"supportsReasoningEffort":true,"reasoningEfforts":[{"id":"none","default":true}]}}]}}`))
	if err != nil || catalog[0].ReasoningKnown || !catalog[1].ReasoningKnown || len(catalog[1].ReasoningEfforts) != 0 || catalog[2].ReasoningEfforts[0].Value != "none" {
		t.Fatalf("%+v %v", catalog, err)
	}
}

// insideWorkdir compares resolved paths case-insensitively, because Windows
// may report the same temp directory in short (RUNNER~1) and long form.
func insideWorkdir(path string) bool {
	dir, err := os.Getwd()
	if err != nil || path == "" {
		return false
	}
	dir, _ = filepath.EvalSymlinks(dir)
	path, _ = filepath.EvalSymlinks(path)
	prefix := dir + string(os.PathSeparator)
	return len(path) > len(prefix) && strings.EqualFold(path[:len(prefix)], prefix)
}

// The scheme's max output reaches Grok Build as max_completion_tokens for the
// run's model in the isolated config.toml (it used to be ignored).
func TestMaxOutputReachesGrokConfig(t *testing.T) {
	a := fixture(t).(*Adapter)
	a.config.MaxOutputTokens = 3000
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if _, err := a.Execute(ctx, lmbp.TaskRun{Model: "m", Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hello\nexpect-max-out=3000")}}}, &backend.Emitter{Emit: func(lmbp.TaskDelta) error { return nil }}); err != nil {
		t.Fatal(err)
	}
}
