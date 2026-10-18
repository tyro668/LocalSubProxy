package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

var weatherTool = []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}}}

func user(content any) []any { return []any{map[string]any{"role": "user", "content": content}} }

func TestE2EBasicAndListing(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	_, m := r.api(t, "GET", "/lmbp/v1/tenant/devices", nil, nil)
	d := m["devices"].([]any)[0].(map[string]any)
	if d["jkt"] != r.jkt || d["e2ee_key"] == nil || d["max_concurrency"].(float64) != 4 {
		t.Fatalf("device listing: %v", d)
	}
	code, m := r.api(t, "POST", "/v1/chat/completions", map[string]any{"model": r.model("echo"), "messages": user("hello there")}, nil)
	if code != 200 || m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "hello there" {
		t.Fatalf("basic: %d %v", code, m)
	}
}

func TestE2ETools(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	resp := doReq(t, r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("tool"), "stream": true, "tools": weatherTool, "messages": user("weather in Paris?")}, nil))
	evs := readSSE(resp.Body, nil)
	resp.Body.Close()
	var args strings.Builder
	name := ""
	for _, e := range evs {
		var c struct {
			Choices []struct {
				Delta struct {
					ToolCalls []lmbp.ToolCall `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal([]byte(e.data), &c)
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				name += tc.Function.Name
				args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if _, fin := textOf(evs); fin != "tool_calls" || name != "get_weather" || args.String() != `{"city":"Paris"}` {
		t.Fatalf("tool call stream: %s %s %v", name, args.String(), evs)
	}
	code, m := r.api(t, "POST", "/v1/chat/completions", map[string]any{"model": r.model("tool"), "tools": weatherTool, "messages": []any{
		map[string]any{"role": "user", "content": "weather in Paris?"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"Paris"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "18°C"},
	}}, nil)
	if code != 200 || m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "It is 18°C" {
		t.Fatalf("tool result: %d %v", code, m)
	}
	_, m = r.api(t, "POST", "/v1/chat/completions", map[string]any{"model": r.model("tool"), "tools": weatherTool, "messages": user("x")}, nil)
	ch := m["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "tool_calls" || ch["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("aggregated tool call: %v", m)
	}
}

func TestE2EImageAndCapabilities(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	img := make([]byte, 200<<10) // > 16 KiB → blob, 3 chunks of ≤96 KiB
	_, _ = rand.Read(img)
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)
	code, m := r.api(t, "POST", "/v1/chat/completions", map[string]any{"model": r.model("echo"), "messages": user([]any{
		map[string]any{"type": "text", "text": "what is this"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}}})}, nil)
	if code != 200 || m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != fmt.Sprintf("image:image/png:%d;what is this", len(img)) {
		t.Fatalf("image echo: %d %v", code, m)
	}
	img2 := func(u string) []any {
		return user([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}})
	}
	for i, c := range []struct {
		body map[string]any
		code int
		err  string
	}{
		{map[string]any{"model": r.model("slow"), "tools": weatherTool, "messages": user("x")}, 400, lmbp.ErrUnsupported},
		{map[string]any{"model": r.model("tool"), "messages": img2("https://example.com/a.png")}, 400, lmbp.ErrUnsupported},
		{map[string]any{"model": r.model("echo"), "messages": img2("http://example.com/a.png")}, 400, lmbp.ErrBadRequest},
		{map[string]any{"model": r.model("echo"), "messages": img2("data:image/png;base64," + strings.Repeat("A", 28<<20))}, 413, lmbp.ErrPayloadTooLarge},
	} {
		code, m := r.api(t, "POST", "/v1/chat/completions", c.body, nil)
		e, _ := m["error"].(map[string]any)
		if code != c.code || e["code"] != c.err {
			t.Fatalf("case %d: expected %d %s, got %d %v", i, c.code, c.err, code, m)
		}
	}
}

func TestE2EResumeNoLossNoDup(t *testing.T) {
	const n = 120
	r := newRig(t, lmbp.Flow{}, n, 5*time.Millisecond)
	resp := doReq(t, r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("slow"), "stream": true, "messages": user("go")}, nil))
	defer resp.Body.Close()
	taskID := resp.Header.Get("X-Task-Id")
	got := 0
	evs := readSSE(resp.Body, func(e sse) bool {
		got++
		if got == 30 || got == 70 {
			r.dev.Disconnect() // drop the device WebSocket mid-stream, twice
		}
		return false
	})
	text, fin := textOf(evs)
	var want strings.Builder
	for i := 0; i < n; i++ {
		want.WriteString(slowChunk(i))
	}
	if fin != "stop" || text != want.String() {
		t.Fatalf("resume lost/duplicated output: finish=%q len=%d want=%d", fin, len(text), want.Len())
	}
	if r.exec.count(taskID) != 1 {
		t.Fatal("task executed more than once")
	}
	if r.dev.Resumes() < 2 || r.g.Resumes() < 2 {
		t.Fatalf("expected 2 resumes, got device=%d gateway=%d", r.dev.Resumes(), r.g.Resumes())
	}
	last := -1
	for _, e := range evs {
		if e.id == "" {
			continue
		}
		var id int
		fmt.Sscan(e.id, &id)
		if id <= last {
			t.Fatalf("non-monotonic SSE id %d after %d", id, last)
		}
		last = id
	}
}

func TestE2EBackpressure(t *testing.T) {
	const n = 20000 // ≈6 MB: far more than socket buffers can absorb
	// The gateway advertises a tiny receive window (8 KiB/task) to the device.
	r := newRig(t, lmbp.Flow{TaskWindow: 8 << 10, ConnWindow: 32 << 10}, n, 0)
	// No wall-clock cap on the whole 6 MB stream (the shared client's 30 s
	// Timeout cut slow -race runs mid-stream); a generous ctx bounds it.
	// 3 minutes was not enough on the Windows runner (ci-macfix: cut at
	// 19459/20000 chunks); this runs in about 5 s on Linux.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	start := time.Now()
	defer cancel()
	resp, err := (&http.Client{}).Do(r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("slow"), "stream": true, "messages": user("go")}, nil).WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReaderSize(resp.Body, 512)
	_, _ = br.ReadString('\n')
	// The client stops reading. Socket buffers fill up, the gateway stops
	// acknowledging, and the device's emit blocks on its credit window.
	// Stalled = the device's task window is full (no room for another
	// chunk) and no chunk was emitted across 20 consecutive observations.
	const frame = 400 // > one slow chunk frame
	var stalled int64 = -1
	stable := 0
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline) && stable < 20; {
		time.Sleep(50 * time.Millisecond)
		_, used := r.dev.OutboxUsage()
		cur := r.exec.emitted.Load()
		if used+frame > 8<<10 && cur == stalled {
			stable++
		} else {
			stable = 0
		}
		stalled = cur
	}
	if stable < 20 {
		t.Fatalf("device never blocked on its credit window (emitted %d)", stalled)
	}
	if stalled >= n {
		t.Fatalf("no backpressure: device emitted all %d chunks while the client was paused", stalled)
	}
	paused := time.Since(start)
	evs := readSSE(br, nil)
	text, fin := textOf(evs)
	if fin != "stop" || strings.Count(text, "[") != n {
		t.Fatalf("stream after backpressure: %q, %d chunks (stalled after %v, then read for %v)", fin, strings.Count(text, "["), paused, time.Since(start)-paused)
	}
	t.Logf("stalled after %v, rest of the stream read in %v", paused, time.Since(start)-paused)
	t.Logf("device paused after %d/%d chunks while the client was not reading", stalled, n)
}

func TestE2EIdempotencyAndLastEventID(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 60, 5*time.Millisecond)
	body := map[string]any{"model": r.model("slow"), "stream": true, "messages": user("go")}
	hdr := map[string]string{"Idempotency-Key": "k-123"}
	resp := doReq(t, r.req("POST", "/v1/chat/completions", body, hdr))
	taskID := resp.Header.Get("X-Task-Id")
	first := readSSE(resp.Body, func(e sse) bool { return e.id == "10" })
	resp.Body.Close() // client drops; the task keeps running during the grace period
	hdr["Last-Event-ID"] = "10"
	resp = doReq(t, r.req("POST", "/v1/chat/completions", body, hdr))
	if resp.Header.Get("X-Task-Id") != taskID {
		t.Fatal("idempotent retry created a new task")
	}
	rest := readSSE(resp.Body, nil)
	resp.Body.Close()
	text, fin := textOf(append(first, rest...))
	if fin != "stop" || strings.Count(text, "[") != 60 {
		t.Fatalf("re-attach lost or duplicated: %q %d chunks", fin, strings.Count(text, "["))
	}
	if r.exec.count(taskID) != 1 {
		t.Fatal("idempotent retry re-executed")
	}
	resp = doReq(t, r.req("GET", "/lmbp/v1/tenant/tasks/"+taskID+"/events", nil, map[string]string{"Last-Event-ID": "55"}))
	tail := readSSE(resp.Body, nil)
	resp.Body.Close()
	if tt, fin := textOf(tail); fin != "stop" || strings.Count(tt, "[") != 5 {
		t.Fatalf("events replay: %q %d", fin, strings.Count(tt, "["))
	}
	body["max_tokens"] = 5
	delete(hdr, "Last-Event-ID")
	if code, m := r.api(t, "POST", "/v1/chat/completions", body, hdr); code != 422 || m["error"].(map[string]any)["code"] != lmbp.ErrIdempotency {
		t.Fatalf("key reuse: %d %v", code, m)
	}
}

func TestE2EEndToEndEncryption(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	_, m := r.api(t, "GET", "/lmbp/v1/tenant/devices", nil, nil)
	d := m["devices"].([]any)[0].(map[string]any)
	pkb, _ := json.Marshal(d["public_key"])
	var pk lmbp.JWK
	_ = json.Unmarshal(pkb, &pk)
	devPub, _ := lmbp.UnB64(pk.X)
	// The SaaS verifies the key statement against the jkt pinned at pairing.
	xpub, kid, err := lmbp.VerifyE2EEKey(devPub, r.jkt, r.devID, d["e2ee_key"].(string), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	secret := "SECRET-PROMPT-71c3 tell me"
	respKey := make([]byte, 32)
	_, _ = rand.Read(respKey)
	pt, _ := json.Marshal(lmbp.E2EERequest{RID: "r_1", Iat: time.Now().Unix(), Model: "echo", Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text(secret)}}, ResponseKey: lmbp.B64(respKey)})
	jwe, err := lmbp.SealJWE(xpub, kid, pt)
	if err != nil {
		t.Fatal(err)
	}
	resp := doReq(t, r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("echo"), "stream": true, "lmbp_e2ee": jwe}, nil))
	evs := readSSE(resp.Body, nil)
	resp.Body.Close()
	var out strings.Builder
	finish := ""
	for _, e := range evs {
		var c struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
			Usage *lmbp.Usage `json:"usage"`
			E     *struct {
				Seq  int64  `json:"seq"`
				Type string `json:"type"`
				Data string `json:"data"`
			} `json:"lmbp_e2ee"`
		}
		_ = json.Unmarshal([]byte(e.data), &c)
		if len(c.Choices) > 0 && c.Choices[0].Delta["content"] != nil && c.Choices[0].Delta["content"] != "" {
			t.Fatalf("cleartext content passed through gateway: %s", e.data)
		}
		if c.E == nil {
			continue
		}
		p, err := lmbp.OpenChunk(respKey, "r_1", c.E.Type, c.E.Seq, c.E.Data)
		if err != nil {
			t.Fatal(err)
		}
		if c.E.Type == lmbp.TypeTaskDelta {
			var dl lmbp.TaskDelta
			_ = json.Unmarshal(p, &dl)
			out.WriteString(dl.Text)
		} else {
			var dn map[string]string
			_ = json.Unmarshal(p, &dn)
			finish = dn["finish_reason"]
			if c.Usage == nil || c.Usage.CompletionTokens != 4 {
				t.Fatal("usage must stay cleartext for accounting")
			}
		}
	}
	if out.String() != secret || finish != "stop" {
		t.Fatalf("e2ee round trip: %q %q", out.String(), finish)
	}
	code, m := r.api(t, "POST", "/v1/chat/completions", map[string]any{"model": r.model("echo"), "lmbp_e2ee": jwe}, nil)
	if e, _ := m["error"].(map[string]any); code == 200 || e["code"] != lmbp.ErrDecrypt {
		t.Fatalf("replayed e2ee request: %d %v", code, m)
	}
	time.Sleep(100 * time.Millisecond)
	for _, f := range []string{r.dbPath, r.dbPath + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && (bytes.Contains(b, []byte("SECRET-PROMPT")) || bytes.Contains(b, []byte("tell me"))) {
			t.Fatalf("plaintext found in %s", f)
		}
	}
	if strings.Contains(r.logs.String(), "SECRET-PROMPT") {
		t.Fatal("plaintext found in gateway logs")
	}
}

func TestE2EConcurrencyQueue(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 20, 10*time.Millisecond)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < 8; i++ { // 8 requests, device max_concurrency 4: FIFO queue, none rejected
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := httpc.Do(r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("slow"), "messages": user("go")}, nil))
			if err == nil {
				if resp.StatusCode == 200 {
					ok.Add(1)
				}
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 8 {
		t.Fatalf("queued requests failed: %d/8 ok", ok.Load())
	}
}

// Review7 #6: an E2EE task that succeeded is replayed (idempotency key)
// after the in-memory hub is gone, as after a restart: the replay must still
// carry the sealed frames, not render them as cleartext / empty.
func TestE2EEReplayAfterRestartStaysSealed(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	_, m := r.api(t, "GET", "/lmbp/v1/tenant/devices", nil, nil)
	d := m["devices"].([]any)[0].(map[string]any)
	pkb, _ := json.Marshal(d["public_key"])
	var pk lmbp.JWK
	_ = json.Unmarshal(pkb, &pk)
	devPub, _ := lmbp.UnB64(pk.X)
	xpub, kid, err := lmbp.VerifyE2EEKey(devPub, r.jkt, r.devID, d["e2ee_key"].(string), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	respKey := make([]byte, 32)
	_, _ = rand.Read(respKey)
	pt, _ := json.Marshal(lmbp.E2EERequest{RID: "r_9", Iat: time.Now().Unix(), Model: "echo", Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("sealed hi")}}, ResponseKey: lmbp.B64(respKey)})
	jwe, _ := lmbp.SealJWE(xpub, kid, pt)
	body := map[string]any{"model": r.model("echo"), "lmbp_e2ee": jwe}
	hdr := map[string]string{"Idempotency-Key": "e2ee-replay-1"}
	code, first := r.api(t, "POST", "/v1/chat/completions", body, hdr)
	if code != 200 || first["lmbp_e2ee"] == nil {
		t.Fatalf("first: %d %v", code, first)
	}
	r.g.mu.Lock()
	r.g.tasks = map[string]*gtask{} // restart: only storage is left
	r.g.mu.Unlock()
	code, again := r.api(t, "POST", "/v1/chat/completions", body, hdr)
	if code != 200 || again["lmbp_e2ee"] == nil {
		t.Fatalf("replay after restart lost E2EE: %d %v", code, again)
	}
	fb, _ := json.Marshal(first["lmbp_e2ee"])
	ab, _ := json.Marshal(again["lmbp_e2ee"])
	if string(fb) != string(ab) {
		t.Fatalf("replay differs:\n%s\n%s", fb, ab)
	}
}
