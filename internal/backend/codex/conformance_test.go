package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/cliruntime"
	"github.com/tyro668/avora/internal/backend/clitest"
	"github.com/tyro668/avora/internal/lmbp"
)

// The test binary doubles as a scripted Codex app-server.
func TestMain(m *testing.M) {
	clitest.RunGrandchild()
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("codex-cli 0.0.0-fake")
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "app-server" {
		os.Exit(fakeAppServer())
	}
	os.Exit(m.Run())
}

type rpcOut struct{ w io.Writer }

func (o rpcOut) send(v any) { b, _ := json.Marshal(v); fmt.Fprintf(o.w, "%s\n", b) }
func (o rpcOut) notify(method string, params any) {
	o.send(map[string]any{"method": method, "params": params})
}

func fakeAppServer() int {
	out := rpcOut{os.Stdout}
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 64<<10), 1<<20)
	for scan.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &msg) != nil || len(msg.ID) == 0 {
			continue
		}
		result := func(v any) { out.send(map[string]any{"id": msg.ID, "result": v}) }
		switch msg.Method {
		case "account/read":
			result(map[string]any{"account": map[string]string{"type": "chatgpt"}})
		case "model/list":
			// "open" predates supportedReasoningEfforts in the catalog.
			result(json.RawMessage(`{"data":[{"id":"m","defaultReasoningEffort":"low","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"high"}]},{"id":"open"}]}`))
		case "thread/start":
			result(map[string]any{"thread": map[string]string{"id": "thread_1"}, "modelProvider": "openai"})
		case "turn/start":
			var p struct {
				Effort *string `json:"effort"`
				Input  []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			text := ""
			if len(p.Input) > 0 {
				text = p.Input[0].Text
			}
			result(map[string]any{"turn": map[string]string{"id": "turn_1"}})
			if d, ok := clitest.Find(text); ok {
				return clitest.Play(d, os.Stdout, &wire{})
			}
			if _, want, ok := strings.Cut(text, "expect-effort="); ok {
				got := "default"
				if p.Effort != nil {
					got = *p.Effort
				}
				if got != strings.Fields(want)[0] {
					return 48
				}
			}
			w := &wire{}
			w.Text(os.Stdout, "ok")
			w.End(os.Stdout, "end_turn", 1, 1)
		default:
			result(map[string]any{})
		}
	}
	return 0
}

// wire speaks the app-server notifications of one turn.
type wire struct{ text strings.Builder }

func (*wire) Manifest(io.Writer) {}
func (*wire) Exposed(io.Writer)  {}
func (x *wire) Text(w io.Writer, s string) {
	o := rpcOut{w}
	if x.text.Len() == 0 {
		o.notify("item/started", map[string]any{"turnId": "turn_1", "item": map[string]string{"id": "item_1", "type": "agentMessage", "phase": "final_answer"}})
	}
	x.text.WriteString(s)
	o.notify("item/agentMessage/delta", map[string]string{"turnId": "turn_1", "itemId": "item_1", "delta": s})
}
func (x *wire) End(w io.Writer, stop string, in, out int) {
	o := rpcOut{w}
	if stop == "cancelled" {
		o.notify("turn/completed", map[string]any{"threadId": "thread_1", "turn": map[string]string{"id": "turn_1", "status": "interrupted"}})
		return
	}
	o.notify("item/completed", map[string]any{"turnId": "turn_1", "item": map[string]string{"id": "item_1", "type": "agentMessage", "phase": "final_answer", "text": x.text.String()}})
	o.notify("thread/tokenUsage/updated", map[string]any{"threadId": "thread_1", "turnId": "turn_1", "tokenUsage": map[string]any{"total": map[string]int{"inputTokens": in, "outputTokens": out}}})
	o.notify("turn/completed", map[string]any{"threadId": "thread_1", "turn": map[string]string{"id": "turn_1", "status": "completed"}})
}
func (*wire) ToolCall(w io.Writer) {
	rpcOut{w}.notify("item/started", map[string]any{"turnId": "turn_1", "item": map[string]string{"id": "tool_1", "type": "commandExecution"}})
}
func (*wire) Unknown(io.Writer) {}

func fixture(effort string) backend.Backend {
	return Definition().New(backend.Config{Type: "codex_app_server", CodexBin: os.Args[0], Models: []string{"m"}, Enabled: true, ReasoningEffort: effort}, backend.Dependencies{})
}

func TestConformance(t *testing.T) {
	clitest.Run(t, clitest.Harness{
		Open:  func(*testing.T) backend.Backend { return fixture("") },
		Model: "m",
		EnvAllowed: func(k string) bool {
			return cliruntime.EnvAllowed(k) || k == "CODEX_HOME"
		},
		NotApplicable: map[string]string{
			"length":       "a Codex turn has no max-token status",
			"after_end":    "Execute stops reading at turn/completed and closes the app-server",
			"exit_failure": "the app-server is closed by Avora after turn/completed; its exit status is not a result",
			"unknown":      "app-server notifications are open-ended; unknown ones are ignored by design",
			"exposed":      "Codex has no tool inventory event; tools are disabled by -c flags",
			"no_manifest":  "Codex has no tool inventory event; tools are disabled by -c flags",
		},
	})
}

func execute(t *testing.T, model, effort, prompt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := fixture(effort).Execute(ctx, lmbp.TaskRun{Model: model, Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text(prompt)}}}, &backend.Emitter{Emit: func(lmbp.TaskDelta) error { return nil }})
	return err
}

// A catalog without supportedReasoningEfforts cannot confirm or refuse an
// effort: Codex passes it through, as before catalogs carried efforts.
func TestEffortPassthroughWithoutCatalogEfforts(t *testing.T) {
	if err := execute(t, "open", "low", "expect-effort=low"); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "open", "off", "expect-effort=none"); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "m", "high", "expect-effort=high"); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "m", "medium", "x"); err == nil || !strings.Contains(err.Error(), "CAPABILITY_UNSUPPORTED") {
		t.Fatalf("an effort the catalog refuses was sent: %v", err)
	}
}

func TestProbeReportsVersion(t *testing.T) {
	h := fixture("").Probe(context.Background())
	if h.Health != "ready" || h.AuthState != "ready" || !strings.Contains(h.RuntimeVersion, "fake") {
		t.Fatalf("%+v", h)
	}
}
