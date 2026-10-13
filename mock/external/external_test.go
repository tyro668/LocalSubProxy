// Package external runs the real Go desktop connector end to end against an
// externally started LSP2 gateway (the Node or Python mocks, or any other
// implementation). It is skipped unless LSP2_EXTERNAL_GATEWAY is set:
//
//	LSP2_EXTERNAL_GATEWAY=http://127.0.0.1:8080 LSP2_TENANT=acme LSP2_TENANT_KEY=... \
//	  go test ./mock/external -run TestExternalGateway -v
package external

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/audit"
	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/lsp2"
	"github.com/tyro668/LocalSubProxy/internal/lsp2/client"
	"github.com/tyro668/LocalSubProxy/mock/fakemodel"
)

const dataKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

type env struct {
	t                  *testing.T
	ctx                context.Context
	gw, tenant, key, u string
}

func (e *env) call(method, path string, body any, hdr ...string) (int, http.Header, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(e.ctx, method, e.gw+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("X-End-User", e.u)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, r.Header, b
}

func (e *env) link() string {
	return lsp2.PairLink{Gateway: e.gw, Tenant: e.tenant}.String()
}

func sseText(b []byte) (string, bool) {
	var sb strings.Builder
	done := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if line == "[DONE]" {
			done = true
			continue
		}
		var ch struct {
			Choices []struct{ Delta struct{ Content string } }
		}
		if json.Unmarshal([]byte(line), &ch) == nil && len(ch.Choices) > 0 {
			sb.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	return sb.String(), done
}

func TestExternalGateway(t *testing.T) {
	gw := strings.TrimRight(os.Getenv("LSP2_EXTERNAL_GATEWAY"), "/")
	if gw == "" {
		t.Skip("LSP2_EXTERNAL_GATEWAY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	e := &env{t: t, ctx: ctx, gw: gw, tenant: os.Getenv("LSP2_TENANT"), key: os.Getenv("LSP2_TENANT_KEY"), u: "e2e-" + lsp2.RandomID("")[:8]}
	if e.tenant == "" {
		e.tenant = "acme"
	}

	// 1. Consent: the signed metadata is verified before anything is shown.
	req, err := client.PrepareLink(ctx, nil, e.link())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !req.Consent.Verified || req.Consent.SaaS.Name == "" || req.Consent.Gateway != gw || req.Consent.Audience != lsp2.TokenAudience(gw, e.tenant) {
		t.Fatalf("consent %+v", req.Consent)
	}
	// Local refusal never reaches the gateway and reports CONSENT_DENIED.
	if err := req.Deny(); !client.IsConsentDenied(err) {
		t.Fatalf("local deny: %v", err)
	}
	if _, err := req.Approve(ctx, "e2e"); err == nil {
		t.Fatalf("approve after local deny: %v", err)
	}

	// 2. SaaS-side denial: the code is dead and re-approving yields CONSENT_DENIED.
	req, _ = client.PrepareLink(ctx, nil, e.link())
	pd, err := req.Approve(ctx, "e2e-denied")
	if err != nil {
		t.Fatal(err)
	}
	if st, _, b := e.call("POST", "/v2/tenant/device-approvals", map[string]any{"user_code": pd.UserCode, "deny": true}); st != 200 {
		t.Fatalf("deny: %d %s", st, b)
	}
	st, h, b := e.call("POST", "/v2/tenant/device-approvals", map[string]any{"user_code": pd.UserCode})
	var prob map[string]any
	_ = json.Unmarshal(b, &prob)
	if st != 403 || prob["code"] != lsp2.ErrConsentDenied || !strings.HasPrefix(h.Get("Content-Type"), "application/problem+json") || prob["status"] != float64(403) {
		t.Fatalf("re-approve denied code: %d %s %s", st, h.Get("Content-Type"), b)
	}
	if _, err := pd.Wait(ctx); err == nil {
		t.Fatal("denied pairing produced an identity")
	}

	// 3. Real pairing with the E2EE key pin (jkt).
	req, _ = client.PrepareLink(ctx, nil, e.link())
	p, err := req.Approve(ctx, "e2e-connector")
	if err != nil {
		t.Fatal(err)
	}
	if st, _, b := e.call("POST", "/v2/tenant/device-approvals", map[string]any{"user_code": p.UserCode, "jkt": p.JKT}); st != 200 {
		t.Fatalf("approve: %d %s", st, b)
	}
	id, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if id.Issuer != gw || id.Audience != lsp2.TokenAudience(gw, e.tenant) {
		t.Fatalf("identity binding %+v", id)
	}

	// 4. Connector with a local OpenAI-compatible model (tools + vision).
	model := httptest.NewServer((&fakemodel.Server{Models: []string{"m1"}}).Handler())
	defer model.Close()
	dir := t.TempDir()
	al, err := audit.Open(filepath.Join(dir, "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	c, err := connector.New(connector.Config{Identity: &id, DataKey: dataKey, AdapterFile: filepath.Join(dir, "adapters.enc"), Audit: al})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAdapter(connector.AdapterConfig{ID: "lm", Type: "openai_compatible", BaseURL: model.URL, APIKey: "local", Models: []string{"m1"}, Capabilities: []string{"tools", "vision"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = c.Run(runCtx) }()
	full := id.DeviceID + "/lm/m1"
	for i := 0; ; i++ {
		_, _, b := e.call("GET", "/v1/models", nil)
		if strings.Contains(string(b), full) {
			break
		}
		if i > 100 {
			t.Fatalf("model never appeared: %s", b)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 4a. Streaming SSE.
	st, _, b = e.call("POST", "/v1/chat/completions", map[string]any{"model": full, "stream": true, "messages": []map[string]string{{"role": "user", "content": "ping"}}})
	if txt, done := sseText(b); st != 200 || !done || !strings.Contains(txt, "ping") {
		t.Fatalf("stream %d done=%v %q", st, done, txt)
	}
	// 4b. Tools (non-streaming aggregation of streamed tool_call fragments).
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}}}
	st, _, b = e.call("POST", "/v1/chat/completions", map[string]any{"model": full, "tools": tools, "messages": []map[string]string{{"role": "user", "content": "weather in Paris?"}}})
	var cc struct {
		Choices []struct {
			Message      lsp2.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(b, &cc)
	if st != 200 || len(cc.Choices) != 1 || cc.Choices[0].FinishReason != "tool_calls" || len(cc.Choices[0].Message.ToolCalls) != 1 || cc.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tools %d %s", st, b)
	}
	// 4c. Multimodal: a 40 KiB image travels as blob.chunk frames.
	img := make([]byte, 40<<10)
	_, _ = rand.Read(img)
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)
	st, _, b = e.call("POST", "/v1/chat/completions", map[string]any{"model": full, "stream": true, "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "what is this"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}}}}}})
	if txt, done := sseText(b); st != 200 || !done || !strings.Contains(txt, "40960") {
		t.Fatalf("image %d done=%v %q", st, done, txt)
	}
	// 4d. E2EE: seal to the device key published (and signed by the device) in the device list.
	_, _, b = e.call("GET", "/v2/tenant/devices", nil)
	var dl struct {
		Devices []struct {
			DeviceID  string   `json:"device_id"`
			JKT       string   `json:"jkt"`
			PublicKey lsp2.JWK `json:"public_key"`
			E2EEKey   string   `json:"e2ee_key"`
		} `json:"devices"`
	}
	_ = json.Unmarshal(b, &dl)
	var xpub []byte
	var kid string
	for _, d := range dl.Devices {
		if d.DeviceID == id.DeviceID {
			pub, _ := lsp2.UnB64(d.PublicKey.X)
			xpub, kid, err = lsp2.VerifyE2EEKey(pub, p.JKT, id.DeviceID, d.E2EEKey, time.Now())
		}
	}
	if err != nil || len(xpub) != 32 {
		t.Fatalf("e2ee key statement: %v %s", err, b)
	}
	rkey := make([]byte, 32)
	_, _ = rand.Read(rkey)
	rid := lsp2.RandomID("rid_")
	plain, _ := json.Marshal(lsp2.E2EERequest{RID: rid, Iat: time.Now().Unix(), Model: "lm/m1", Messages: []lsp2.Message{{Role: "user", Content: lsp2.Text("sealed hello")}}, ResponseKey: lsp2.B64(rkey)})
	jwe, _ := lsp2.SealJWE(xpub, kid, plain)
	st, _, b = e.call("POST", "/v1/chat/completions", map[string]any{"model": full, "stream": true, "lsp2_e2ee": jwe})
	if st != 200 || bytes.Contains(b, []byte("sealed hello")) {
		t.Fatalf("e2ee %d leak=%v", st, bytes.Contains(b, []byte("sealed hello")))
	}
	var opened strings.Builder
	frames := 0
	for _, line := range strings.Split(string(b), "\n") {
		var ch struct {
			E struct {
				Seq  int64  `json:"seq"`
				Type string `json:"type"`
				Data string `json:"data"`
			} `json:"lsp2_e2ee"`
		}
		if l, ok := strings.CutPrefix(line, "data: "); ok && json.Unmarshal([]byte(l), &ch) == nil && ch.E.Data != "" {
			pt, err := lsp2.OpenChunk(rkey, rid, ch.E.Type, ch.E.Seq, ch.E.Data)
			if err != nil {
				t.Fatalf("open chunk: %v", err)
			}
			frames++
			var d lsp2.TaskDelta
			_ = json.Unmarshal(pt, &d)
			opened.WriteString(d.Text)
		}
	}
	if frames < 2 || !strings.Contains(opened.String(), "sealed hello") {
		t.Fatalf("e2ee frames %d text %q", frames, opened.String())
	}
	// 4e. Problem details on the gateway surface; OpenAI errors on /v1.
	st, h, b = e.call("GET", "/v2/tenant/tasks/task_nope/events", nil)
	_ = json.Unmarshal(b, &prob)
	if st != 404 || !strings.HasPrefix(h.Get("Content-Type"), "application/problem+json") || prob["type"] == nil || prob["title"] == nil {
		t.Fatalf("problem %d %s", st, b)
	}

	// 5. The local audit log attributes every task to this SaaS, without content.
	var recs []audit.Record
	for i := 0; i < 100 && len(recs) < 4; i++ {
		recs, _ = c.AuditList(ctx, audit.Filter{})
		time.Sleep(30 * time.Millisecond)
	}
	if len(recs) != 4 {
		t.Fatalf("audit records %d", len(recs))
	}
	for _, r := range recs {
		if r.Tenant != e.tenant || r.Gateway != gw || r.ConnectionID != id.DeviceID {
			t.Fatalf("audit %+v", r)
		}
	}
	if j, _ := json.Marshal(recs); bytes.Contains(j, []byte("ping")) || bytes.Contains(j, []byte("sealed hello")) {
		t.Fatal("audit log contains prompt content")
	}

	// 5b. Anthropic Messages through the real connector: streaming text and a tool round trip.
	st, _, b = e.call("POST", "/v1/messages", map[string]any{"model": full, "max_tokens": 64, "stream": true, "system": "sys", "messages": []map[string]string{{"role": "user", "content": "ping"}}})
	if st != 200 || !bytes.Contains(b, []byte("event: message_start")) || !bytes.Contains(b, []byte("event: message_stop")) || !bytes.Contains(b, []byte(`"stop_reason":"end_turn"`)) {
		t.Fatalf("messages stream %d %s", st, b)
	}
	atools := []map[string]any{{"name": "get_weather", "input_schema": map[string]any{"type": "object"}}}
	st, _, b = e.call("POST", "/v1/messages", map[string]any{"model": full, "max_tokens": 64, "tools": atools, "messages": []map[string]string{{"role": "user", "content": "weather in Paris?"}}})
	var am struct {
		StopReason string           `json:"stop_reason"`
		Content    []map[string]any `json:"content"`
	}
	_ = json.Unmarshal(b, &am)
	if st != 200 || am.StopReason != "tool_use" || len(am.Content) == 0 || am.Content[len(am.Content)-1]["type"] != "tool_use" {
		t.Fatalf("messages tools %d %s", st, b)
	}
	// 5c. Optional: the official openai/anthropic client libraries, unchanged,
	// with base_url pointed at the gateway (scripts/official-clients.sh).
	if cmd := os.Getenv("LSP2_OFFICIAL_CLIENTS"); cmd != "" {
		x := exec.CommandContext(ctx, "bash", "-c", cmd)
		x.Env = append(os.Environ(), "LSP2_BASE_URL="+gw, "LSP2_API_KEY="+e.key, "LSP2_END_USER="+e.u, "LSP2_MODEL="+full)
		out, err := x.CombinedOutput()
		t.Logf("official clients:\n%s", out)
		if err != nil {
			t.Fatalf("official clients: %v", err)
		}
	}

	// 6. Refresh via RFC 7523 assertion happens inside the connector; force a
	// reconnect by revoking and check the session ends and refresh is refused.
	st, _, _ = e.call("POST", "/v2/tenant/devices/"+url.PathEscape(id.DeviceID)+"/revoke", nil)
	if st != 200 {
		t.Fatalf("revoke %d", st)
	}
	for i := 0; i < 100 && c.Status()["gateway_connected"] == true; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if c.Status()["gateway_connected"] == true {
		t.Fatal("still connected after revoke")
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
