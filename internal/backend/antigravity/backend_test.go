package antigravity

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
	if len(os.Args) > 1 && (os.Args[1] == "--help" || os.Args[1] == "models" || os.Args[1] == "--agent") {
		fakeCLI()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func fakeCLI() {
	if os.Args[1] == "--help" {
		fmt.Fprintln(os.Stderr, "--agent --disable-slash-commands --sandbox --input-format")
		return
	}
	if os.Args[1] == "models" {
		fmt.Println("m\tModel\nsecond\tSecond Model\nflash-high\tFlash (High)\nflash-medium\tFlash (Medium)\nflash-low\tFlash (Low)")
		return
	}
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
	dir, _ := os.Getwd()
	data, err := os.ReadFile(filepath.Join(dir, ".agents", "agents", agentName+".md"))
	if err != nil || string(data) != agentDefinition {
		os.Exit(41)
	}
	tmp, _ := filepath.EvalSymlinks(os.TempDir())
	real, _ := filepath.EvalSymlinks(dir)
	if !has("--sandbox") || !strings.HasPrefix(tmp, real+string(os.PathSeparator)) {
		os.Exit(47)
	}
	model := value("--model")
	out := json.NewEncoder(os.Stdout)
	if name, ok := strings.CutPrefix(model, "sample-"); ok {
		data, err := samples.ReadFile("testdata/" + name + ".jsonl")
		if err != nil {
			os.Exit(48)
		}
		cwd, _ := json.Marshal(dir)
		first, rest, _ := strings.Cut(strings.Replace(string(data), `"/home/user/project"`, string(cwd), 1), "\n")
		fmt.Println(first)
		if !bufio.NewScanner(os.Stdin).Scan() {
			os.Exit(42)
		}
		fmt.Print(rest)
		return
	}
	switch model {
	case "early-step":
		fmt.Println(`{"event":"step_update","step_update":{"conversation_id":"session","step_type":"agent_response","text_delta":"leaked"}}`)
		return
	case "wrong-agent":
		out.Encode(map[string]any{"event": "init", "conversation_id": "session", "init": map[string]any{"agent": "default", "cwd": dir}})
		return
	}
	out.Encode(map[string]any{"event": "init", "conversation_id": "session", "init": map[string]any{"agent": agentName, "cwd": dir, "tools": []string{"run_command", "finish"}}})
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		os.Exit(42)
	}
	var input struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(sc.Bytes(), &input) == nil {
		if d, ok := clitest.Find(input.Message.Content); ok {
			os.Exit(clitest.Play(d, os.Stdout, &wire{}))
		}
	}
	if input.Event != "user" || !strings.HasPrefix(input.Message.Content, "[user]\nhello") {
		os.Exit(43)
	}
	if sc.Scan() {
		os.Exit(44)
	} // EOF is required after exactly one request
	if value("--effort") != "" {
		os.Exit(45)
	} // pinned variants need no override
	if _, expected, ok := strings.Cut(input.Message.Content, "expect-model="); ok {
		if model != strings.Fields(expected)[0] {
			os.Exit(46)
		}
	}
	if model == "rate-limited" || model == "quota" {
		msg := map[string]string{"rate-limited": "429 Too Many Requests: rate limit reached", "quota": "RESOURCE_EXHAUSTED: You have exhausted your quota"}[model]
		out.Encode(map[string]any{"event": "result", "result": map[string]any{"conversation_id": "session", "status": "ERROR", "error": msg}})
		return
	}
	if model == "hang" {
		time.Sleep(time.Hour)
	}
	if model == "truncated" {
		return
	}
	fmt.Println(`{"event":"step_update","step_update":{"conversation_id":"session","step_type":"agent_response","text_delta":"hello world"}}`)
	response := "hello world"
	if model == "conflict" {
		response = "different"
	}
	out.Encode(map[string]any{"event": "result", "result": map[string]any{"conversation_id": "session", "status": "SUCCESS", "response": response, "usage": map[string]int{"input_tokens": 3, "output_tokens": 4}}})
	if model == "exit-failure" {
		os.Exit(2)
	}
}
func fixture(t *testing.T) backend.Backend {
	t.Helper()
	home := t.TempDir()
	return Definition().New(backend.Config{Type: "antigravity_cli", Executable: os.Args[0], Models: []string{"m"}, Enabled: true}, backend.Dependencies{UserHomeDir: func() (string, error) { return home, nil }})
}
func TestProcessContract(t *testing.T) {
	a := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if ids, err := a.Models(ctx); err != nil || len(ids) != 5 {
		t.Fatalf("models %v %v", ids, err)
	}
	var text strings.Builder
	done, err := a.Execute(ctx, lmbp.TaskRun{Model: "m", Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hello")}}}, &backend.Emitter{Emit: func(d lmbp.TaskDelta) error { text.WriteString(d.Text); return nil }})
	if err != nil || done.FinishReason != "stop" || text.String() != "hello world" || done.Usage.PromptTokens != 3 || done.Usage.CompletionTokens != 4 {
		t.Fatalf("result %v %v %q", done, err, text.String())
	}
}
func TestProcessFailures(t *testing.T) {
	for _, mode := range []string{"truncated", "conflict", "exit-failure", "hang", "emit-error"} {
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
func TestStreamRejectsInvalidOrderingAndActions(t *testing.T) {
	for _, line := range []string{`{"event":"step_update","step_update":{"conversation_id":"wrong","step_type":"agent_response","text_delta":"leaked"}}`, `{"event":"step_update","step_update":{"conversation_id":"session","step_type":"tool","tool_name":"run_command"}}`, `{"event":"result","result":{"status":"RUNNING"}}`, `garbage`} {
		s := &stream{initialized: true, session: "session", out: &backend.Emitter{Emit: func(lmbp.TaskDelta) error { t.Fatal("unexpected output"); return nil }}}
		if s.consume([]byte(line)) == nil {
			t.Fatalf("accepted %s", line)
		}
	}
	s := &stream{initialized: true, session: "session", out: &backend.Emitter{Emit: func(lmbp.TaskDelta) error { return nil }}}
	line := []byte(`{"event":"result","result":{"conversation_id":"session","status":"SUCCESS","response":"fallback"}}`)
	if err := s.consume(line); err != nil {
		t.Fatal(err)
	}
	if s.consume(line) == nil {
		t.Fatal("accepted duplicate result")
	}
}
func TestGlobalCustomizationsAreCheckedBeforeLaunch(t *testing.T) {
	for _, tc := range []struct{ path, content string }{
		{".gemini/config/hooks.json", `{"hook":{"SessionStart":[{"command":"touch /tmp/unsafe"}]}}`},
		{".gemini/antigravity-cli/settings.json", `{"hooks":{"Stop":[]},"permissions":{}}`},
		{".gemini/config/mcp_config.json", `{"mcpServers":{"x":{"command":"unsafe"}}}`},
		{".gemini/config/plugins/x/hooks.json", `{}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			home := t.TempDir()
			p := filepath.Join(home, filepath.FromSlash(tc.path))
			os.MkdirAll(filepath.Dir(p), 0700)
			os.WriteFile(p, []byte(tc.content), 0600)
			err := checkCustomizations(home)
			if strings.Contains(tc.content, `"Stop":[]`) {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe customization accepted")
			}
		})
	}
	home := t.TempDir()
	p := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, []byte(`{"mcpServers":{}}`), 0600)
	if err := checkCustomizations(home); err != nil {
		t.Fatal(err)
	}
}

func TestEffortUsesAvailableVariants(t *testing.T) {
	text := "flash-high\tFlash (High)\nflash-low\tFlash (Low)\ngpt-oss-120b-medium\tGPT-OSS 120B (Medium)\ngpt-oss-120b-medium\tGPT-OSS 120B (Medium)\nclaude-thinking\tClaude (Thinking)\nother-high\tDifferent\nother-low\tUnrelated"
	models, err := parseCatalog(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(models[0].ReasoningEfforts) != 2 || models[0].DefaultReasoningEffort != "high" || models[0].ReasoningEfforts[0].Model != "flash-low" {
		t.Fatalf("%+v", models[0])
	}
	for _, m := range models[2:] {
		if len(m.ReasoningEfforts) != 0 {
			t.Fatalf("invented capability: %+v", m)
		}
	}
}

func TestReasoningVariantWireModel(t *testing.T) {
	for _, tc := range []struct{ model, effort, want string }{
		{"flash-high", "", "flash-high"}, {"flash-high", "default", "flash-high"},
		{"flash-high", "low", "flash-low"}, {"flash-low", "medium", "flash-medium"}, {"flash-low", "high", "flash-high"},
		{"flash-high", "none", ""}, {"flash-high", "xhigh", ""}, {"m", "low", ""}, {"custom", "low", ""},
	} {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			a := fixture(t).(*Adapter)
			a.config.ReasoningEffort = tc.effort
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			_, err := a.Execute(ctx, lmbp.TaskRun{Model: tc.model, Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hello\nexpect-model=" + tc.want)}}}, &backend.Emitter{Emit: func(lmbp.TaskDelta) error { return nil }})
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "CAPABILITY_UNSUPPORTED") {
					t.Fatalf("expected capability error, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
