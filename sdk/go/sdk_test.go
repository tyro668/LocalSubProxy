package lsp2server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lsp2"
	"github.com/tyro668/LocalSubProxy/internal/lsp2/client"
	lsp2server "github.com/tyro668/LocalSubProxy/sdk/go"
)

const key = "tk_ssssssssssssssssssssssssssssssssssssssss"

// echo answers "echo:<last user text>" in two deltas, or calls tool "lookup" when tools are offered.
type echo struct{}

func (echo) Models() []string { return []string{"m1"} }
func (echo) ModelCapabilities() map[string][]string {
	return map[string][]string{"m1": {"tools"}}
}
func (echo) Execute(ctx context.Context, run lsp2.TaskRun, emit func(lsp2.TaskDelta) error) (*lsp2.TaskDone, *lsp2.ErrorBody) {
	if len(run.Tools) > 0 && run.Messages[len(run.Messages)-1].Role != "tool" {
		_ = emit(lsp2.TaskDelta{ToolCalls: []lsp2.ToolCall{{ID: "call_1", Type: "function", Function: lsp2.FunctionCall{Name: "lookup", Arguments: `{"q":"x"}`}}}})
		return &lsp2.TaskDone{FinishReason: "tool_calls"}, nil
	}
	txt := "echo:" + run.Messages[len(run.Messages)-1].Content.PlainText()
	_ = emit(lsp2.TaskDelta{Text: txt[:3]})
	_ = emit(lsp2.TaskDelta{Text: txt[3:]})
	return &lsp2.TaskDone{FinishReason: "stop", Usage: &lsp2.Usage{PromptTokens: 1, CompletionTokens: 2}}, nil
}

type rig struct {
	srv *lsp2server.Server
	hs  *httptest.Server
	dev *client.Client
}

func newRig(t *testing.T, st lsp2server.Store) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := lsp2server.New(ctx, lsp2server.Options{Store: st, Logger: log.New(io.Discard, "", 0),
		Tenants: []lsp2server.Tenant{{ID: "acme", APIKey: key, Profile: lsp2server.TenantProfile{DisplayName: "Acme"}}}})
	if err != nil {
		t.Fatal(err)
	}
	app := http.NewServeMux()
	app.HandleFunc("/app/hello", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "app") })
	hs := httptest.NewServer(srv.Middleware(app))
	t.Cleanup(hs.Close)
	return &rig{srv: srv, hs: hs}
}

// pair runs the desktop side (real device client) against the business-side pairing API.
func (r *rig) pair(t *testing.T, user string) string {
	t.Helper()
	ctx := context.Background()
	info, err := r.srv.Pairing.Create("acme", r.hs.URL)
	if err != nil || !strings.HasPrefix(info.Link, "lsp2://pair?") || !strings.HasPrefix(info.QRSVG, "<svg") || len(info.QRPNG) < 100 {
		t.Fatalf("create: %v %+v", err, info.Link)
	}
	req, err := client.PrepareLink(ctx, nil, info.Link)
	if err != nil {
		t.Fatal(err)
	}
	if req.Consent.GatewayKeyID != r.srv.Keys.KID() {
		t.Fatal("kid mismatch")
	}
	p, err := req.Approve(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	pend, err := r.srv.Pairing.Lookup(ctx, "acme", p.UserCode)
	if err != nil || pend.DeviceName != "laptop" || pend.Status != "pending" {
		t.Fatalf("lookup %+v %v", pend, err)
	}
	if _, err := r.srv.Pairing.Approve(ctx, "acme", user, p.UserCode, "wrong-jkt"); !errors.Is(err, lsp2server.ErrJKTMismatch) {
		t.Fatalf("jkt pin not enforced: %v", err)
	}
	res, err := r.srv.Pairing.Approve(ctx, "acme", user, p.UserCode, pend.JKT)
	if err != nil || res.Status != "approved" {
		t.Fatalf("approve %+v %v", res, err)
	}
	id, err := p.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := client.New(client.Config{Executor: echo{}, Logger: log.New(io.Discard, "", 0)}, id)
	if err != nil {
		t.Fatal(err)
	}
	dctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan error, 1)
	go dev.Run(dctx, ready)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	r.dev = dev
	waitOnline(t, r, user, true)
	return res.DeviceID
}

func waitOnline(t *testing.T, r *rig, user string, want bool) {
	for i := 0; i < 100; i++ {
		ds, _ := r.srv.Pairing.ListDevices(context.Background(), "acme", user)
		if len(ds) > 0 && ds[0].Online == want {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("device online != %v", want)
}

func TestEndToEnd(t *testing.T) {
	for name, open := range map[string]func() lsp2server.Store{
		"memory": lsp2server.NewMemoryStore,
		"sqlite": func() lsp2server.Store {
			s, err := lsp2server.OpenSQLiteStore(filepath.Join(t.TempDir(), "g.db"), make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	} {
		t.Run(name, func(t *testing.T) { testEndToEnd(t, open()) })
	}
}

func testEndToEnd(t *testing.T, st lsp2server.Store) {
	ctx := context.Background()
	r := newRig(t, st)
	devID := r.pair(t, "alice")

	// Middleware: app routes pass through, gateway routes are served.
	if b := get(t, r.hs.URL+"/app/hello"); b != "app" {
		t.Fatalf("app route: %q", b)
	}
	var jwks struct{ Keys []lsp2.JWK }
	_ = json.Unmarshal([]byte(get(t, r.hs.URL+"/.well-known/jwks.json")), &jwks)
	if len(jwks.Keys) != 1 || jwks.Keys[0].X != r.srv.Keys.PublicJWK().X {
		t.Fatal("jwks mismatch")
	}

	c, err := r.srv.Client("acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := c.Models(ctx)
	if err != nil || len(ms) != 1 || !strings.HasSuffix(ms[0].ID, "/m1") {
		t.Fatalf("models %+v %v", ms, err)
	}
	model := ms[0].ID
	cc, err := c.ChatCompletion(ctx, lsp2server.ChatRequest{Model: model, Messages: []lsp2.Message{{Role: "user", Content: lsp2.Text("hi")}}})
	if err != nil || cc.Choices[0].Message.Content.PlainText() != "echo:hi" || cc.Choices[0].FinishReason != "stop" {
		t.Fatalf("chat %+v %v", cc, err)
	}
	s, err := c.ChatCompletionStream(ctx, lsp2server.ChatRequest{Model: model, Messages: []lsp2.Message{{Role: "user", Content: lsp2.Text("yo")}}})
	if err != nil {
		t.Fatal(err)
	}
	var txt, fin string
	for s.Next() {
		for _, ch := range s.Current().Choices {
			txt += ch.Delta.Content
			if ch.FinishReason != nil {
				fin = *ch.FinishReason
			}
		}
	}
	if s.Err() != nil || txt != "echo:yo" || fin != "stop" {
		t.Fatalf("stream %q %q %v", txt, fin, s.Err())
	}

	m, err := c.Messages(ctx, lsp2server.MessagesRequest{Model: model, MaxTokens: 64, System: "be brief",
		Messages: []lsp2server.AnthropicMessage{{Role: "user", Content: "ok"}}})
	if err != nil || m.Type != "message" || m.Content[0].Text != "echo:ok" || m.StopReason != "end_turn" {
		t.Fatalf("messages %+v %v", m, err)
	}
	ms2, err := c.MessagesStream(ctx, lsp2server.MessagesRequest{Model: model, MaxTokens: 64,
		Messages: []lsp2server.AnthropicMessage{{Role: "user", Content: "q"}},
		Tools:    []lsp2server.AnthropicTool{{Name: "lookup", InputSchema: map[string]any{"type": "object"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var tool, args, stop string
	for ms2.Next() {
		e := ms2.Current()
		types = append(types, e.Type)
		if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			tool = e.ContentBlock.Name
		}
		args += e.Delta.PartialJSON
		if e.Delta.StopReason != "" {
			stop = e.Delta.StopReason
		}
	}
	if ms2.Err() != nil || types[0] != "message_start" || types[len(types)-1] != "message_stop" || tool != "lookup" || args != `{"q":"x"}` || stop != "tool_use" {
		t.Fatalf("messages stream %v %q %q %q %v", types, tool, args, stop, ms2.Err())
	}

	// Errors surface as APIError.
	_, err = c.ChatCompletion(ctx, lsp2server.ChatRequest{Model: "nope", Messages: []lsp2.Message{{Role: "user", Content: lsp2.Text("x")}}})
	var ae *lsp2server.APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("unknown model: %v", err)
	}
	if _, err := r.srv.Client("other", "alice"); !errors.Is(err, lsp2server.ErrUnknownTenant) {
		t.Fatal("unknown tenant accepted")
	}
	// Other users see nothing.
	bob, _ := r.srv.Client("acme", "bob")
	if ms, _ := bob.Models(ctx); len(ms) != 0 {
		t.Fatal("bob sees alice's models")
	}

	// Revoke ends the session.
	if err := r.srv.Pairing.RevokeDevice(ctx, "acme", "bob", devID); !errors.Is(err, lsp2server.ErrDeviceUnknown) {
		t.Fatalf("revoke by non-owner: %v", err)
	}
	if err := r.srv.Pairing.RevokeDevice(ctx, "acme", "alice", devID); err != nil {
		t.Fatal(err)
	}
	ds, _ := r.srv.Pairing.ListDevices(ctx, "acme", "alice")
	if len(ds) != 1 || ds[0].Status != "revoked" {
		t.Fatalf("after revoke %+v", ds)
	}
	waitOnline(t, r, "alice", false)
}

func TestDenyAndBadCode(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	if _, err := r.srv.Pairing.Approve(ctx, "acme", "alice", "ZZZZ-ZZZZ", ""); !errors.Is(err, lsp2server.ErrCodeInvalid) {
		t.Fatalf("bad code: %v", err)
	}
	info, _ := r.srv.Pairing.Create("acme", r.hs.URL)
	req, err := client.PrepareLink(ctx, nil, info.Link)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := req.Approve(ctx, "pc")
	if res, err := r.srv.Pairing.Deny(ctx, "acme", "alice", p.UserCode); err != nil || res.Status != "denied" {
		t.Fatalf("deny %+v %v", res, err)
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := p.Wait(wctx); !client.IsConsentDenied(err) && err == nil {
		t.Fatal("wait succeeded after deny")
	}
}

func TestSigningKeyPersistsInStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.db")
	kid := func() string {
		st, err := lsp2server.OpenSQLiteStore(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		s, err := lsp2server.New(context.Background(), lsp2server.Options{Store: st, Logger: log.New(io.Discard, "", 0)})
		if err != nil {
			t.Fatal(err)
		}
		return s.Keys.KID()
	}
	if a, b := kid(), kid(); a != b {
		t.Fatal("signing key not persisted")
	}
	if len(lsp2server.GenerateTenantKey()) < 32 {
		t.Fatal("short tenant key")
	}
}

func get(t *testing.T, u string) string {
	r, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}
