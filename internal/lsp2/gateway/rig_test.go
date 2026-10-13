package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"localsubproxy/internal/lsp2"
	"localsubproxy/internal/lsp2/client"
	"localsubproxy/internal/lsp2/store"
)

// fakeExec is a deterministic executor:
//   - "echo": echoes the last user text and describes image inputs;
//   - "tool": calls get_weather unless the last message is a tool result;
//   - "slow": streams slowN numbered chunks with a small delay.
type fakeExec struct {
	execs   sync.Map // task_id → *atomic.Int32
	emitted atomic.Int64
	slowN   int
	delay   time.Duration
}

func (f *fakeExec) Models() []string { return []string{"echo", "tool", "slow"} }
func (f *fakeExec) ModelCapabilities() map[string][]string {
	return map[string][]string{"echo": {"vision", "audio", "tools"}, "tool": {"tools"}, "slow": {}}
}

func slowChunk(i int) string { return fmt.Sprintf("[%04d]%s", i, strings.Repeat("x", 200)) }

func (f *fakeExec) count(taskID string) int32 {
	if n, ok := f.execs.Load(taskID); ok {
		return n.(*atomic.Int32).Load()
	}
	return 0
}

func (f *fakeExec) Execute(ctx context.Context, run lsp2.TaskRun, emit func(lsp2.TaskDelta) error) (*lsp2.TaskDone, *lsp2.ErrorBody) {
	n, _ := f.execs.LoadOrStore(run.TaskID, &atomic.Int32{})
	n.(*atomic.Int32).Add(1)
	send := func(d lsp2.TaskDelta) *lsp2.ErrorBody {
		if err := emit(d); err != nil {
			return &lsp2.ErrorBody{Code: lsp2.ErrCancelled, Message: "cancelled"}
		}
		f.emitted.Add(1)
		return nil
	}
	last := run.Messages[len(run.Messages)-1]
	switch run.Model {
	case "echo":
		for _, p := range last.Content.Parts {
			if p.ImageURL != nil {
				u := p.ImageURL.URL
				mt := u[5:strings.IndexByte(u, ';')]
				raw, _ := base64.StdEncoding.DecodeString(u[strings.IndexByte(u, ',')+1:])
				if e := send(lsp2.TaskDelta{Text: fmt.Sprintf("image:%s:%d;", mt, len(raw))}); e != nil {
					return nil, e
				}
			}
		}
		for _, w := range strings.SplitAfter(last.Content.PlainText(), " ") {
			if e := send(lsp2.TaskDelta{Text: w}); e != nil {
				return nil, e
			}
		}
		return &lsp2.TaskDone{FinishReason: "stop", Usage: &lsp2.Usage{PromptTokens: 3, CompletionTokens: 4}}, nil
	case "tool":
		if last.Role == "tool" {
			if e := send(lsp2.TaskDelta{Text: "It is " + last.Content.PlainText()}); e != nil {
				return nil, e
			}
			return &lsp2.TaskDone{FinishReason: "stop"}, nil
		}
		zero := 0
		if e := send(lsp2.TaskDelta{ToolCalls: []lsp2.ToolCall{{Index: &zero, ID: "call_1", Type: "function", Function: lsp2.FunctionCall{Name: "get_weather", Arguments: `{"city":`}}}}); e != nil {
			return nil, e
		}
		if e := send(lsp2.TaskDelta{ToolCalls: []lsp2.ToolCall{{Index: &zero, Function: lsp2.FunctionCall{Arguments: `"Paris"}`}}}}); e != nil {
			return nil, e
		}
		return &lsp2.TaskDone{FinishReason: "tool_calls"}, nil
	case "slow":
		for i := 0; i < f.slowN; i++ {
			if e := send(lsp2.TaskDelta{Text: slowChunk(i)}); e != nil {
				return nil, e
			}
			select {
			case <-ctx.Done():
				return nil, &lsp2.ErrorBody{Code: lsp2.ErrCancelled, Message: "cancelled"}
			case <-time.After(f.delay):
			}
		}
		return &lsp2.TaskDone{FinishReason: "stop"}, nil
	}
	return nil, &lsp2.ErrorBody{Code: lsp2.ErrModelNotFound, Message: "no model"}
}

type rig struct {
	g      *Gateway
	srv    *httptest.Server
	dev    *client.Client
	exec   *fakeExec
	devID  string
	jkt    string
	dbPath string
	logs   *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

const tenantKey = "tk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func newRig(t *testing.T, flow lsp2.Flow, slowN int, delay time.Duration) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{exec: &fakeExec{slowN: slowN, delay: delay}, dbPath: filepath.Join(dir, "gw.db"), logs: &syncBuf{}}
	st, err := store.OpenSQLite(r.dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(r.logs, "", 0)
	r.g, _ = New(Config{Store: st, Logger: logger, Flow: flow, AttachGrace: 5 * time.Second, PingInterval: time.Second})
	_ = r.g.RegisterTenant(context.Background(), "acme", "Acme", tenantKey)
	mux := http.NewServeMux()
	mux.Handle("/", r.g.Handler())
	var base string
	mux.HandleFunc("/.well-known/lsp-connector", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Discovery{GatewayURL: base, Tenant: "acme", LinkURL: base + "/link", Protocol: "lsp2"})
	})
	r.srv = httptest.NewServer(mux)
	base = r.srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); r.srv.CloseClientConnections(); r.srv.Close(); st.Close() })

	p, err := client.StartPairing(ctx, nil, base, "test-pc")
	if err != nil {
		t.Fatal(err)
	}
	r.jkt = p.JKT
	if !strings.Contains(p.LinkURL, "jkt="+p.JKT) {
		t.Fatalf("link url lacks jkt: %s", p.LinkURL)
	}
	if code, _ := r.api(t, "POST", "/v2/tenant/device-approvals", map[string]string{"user_code": p.UserCode, "jkt": "wrong"}, nil); code != 409 {
		t.Fatalf("approval with wrong jkt: %d", code)
	}
	if code, m := r.api(t, "POST", "/v2/tenant/device-approvals", map[string]string{"user_code": p.UserCode, "jkt": p.JKT}, nil); code != 200 {
		t.Fatalf("approve: %d %v", code, m)
	}
	id, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.devID = id.DeviceID
	r.dev, err = client.New(client.Config{Executor: r.exec, MaxConcurrency: 4, Logger: logger}, id)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go r.dev.Run(ctx, ready)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("device did not connect")
	}
	return r
}

var httpc = &http.Client{Timeout: 30 * time.Second}

func (r *rig) req(method, path string, body any, hdr map[string]string) *http.Request {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+tenantKey)
	req.Header.Set("X-End-User", "alice")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

func doReq(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (r *rig) api(t *testing.T, method, path string, body any, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	resp := doReq(t, r.req(method, path, body, hdr))
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func (r *rig) model(m string) string { return r.devID + "/" + m }

type sse struct {
	id   string
	data string
}

// readSSE parses events until [DONE], EOF, or stop returns true.
func readSSE(body io.Reader, stop func(sse) bool) []sse {
	var out []sse
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	var cur sse
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "id: "):
			cur.id = l[4:]
		case strings.HasPrefix(l, "data: "):
			cur.data = l[6:]
		case l == "" && cur.data != "":
			out = append(out, cur)
			if cur.data == "[DONE]" || (stop != nil && stop(cur)) {
				return out
			}
			cur = sse{}
		}
	}
	return out
}

// textOf concatenates content deltas and returns the finish reason (or
// "error:<code>").
func textOf(evs []sse) (string, string) {
	var b strings.Builder
	finish := ""
	for _, e := range evs {
		var c struct {
			Choices []struct {
				Delta        map[string]any `json:"delta"`
				FinishReason *string        `json:"finish_reason"`
			} `json:"choices"`
			Error map[string]any `json:"error"`
		}
		_ = json.Unmarshal([]byte(e.data), &c)
		if c.Error != nil {
			return b.String(), "error:" + fmt.Sprint(c.Error["code"])
		}
		for _, ch := range c.Choices {
			if s, ok := ch.Delta["content"].(string); ok {
				b.WriteString(s)
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	return b.String(), finish
}
