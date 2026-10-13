// Package demo wires fake model + gateway + two tenants' SaaS + connector
// in-process and walks through pair → connect → stream → cancel → errors →
// isolation → encryption-at-rest → revoke. Used by `lspmock demo` and tests.
package demo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"localsubproxy/internal/lsp2"
	"localsubproxy/internal/lsp2/client"
	"localsubproxy/internal/lsp2/gateway"
	"localsubproxy/internal/lsp2/store"
	"localsubproxy/mock/fakemodel"
	"localsubproxy/mock/saas"
)

type Result struct {
	DeviceID        string
	StreamText      string
	CancelState     string
	UpstreamErrCode string
	NotFoundCode    string
	BobModels       int
	BobRevokeStatus int
	PromptInDB      bool
	RevokedRefresh  bool
	TokenRefreshes  int64
	// Extensions
	ResumeLossless   bool
	Resumes          int64
	BackpressureAt   int64 // deltas the device had emitted when it stalled
	BackpressureAll  int   // chunks finally delivered
	ToolCall         string
	ToolAnswer       string
	ImageReply       string
	E2EEText         string
	E2EEPlainInStore bool
	E2EEPlainInLogs  bool
	E2EECiphertext   bool // the gateway relayed sealed frames only
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// countingExec counts task.delta emissions to make backpressure visible.
type countingExec struct {
	*client.OpenAIExecutor
	n atomic.Int64
}

func (c *countingExec) Execute(ctx context.Context, run lsp2.TaskRun, emit func(lsp2.TaskDelta) error) (*lsp2.TaskDone, *lsp2.ErrorBody) {
	return c.OpenAIExecutor.Execute(ctx, run, func(d lsp2.TaskDelta) error { c.n.Add(1); return emit(d) })
}

func Run(ctx context.Context, out io.Writer, dir string) (*Result, error) {
	res := &Result{}
	say := func(f string, a ...any) { fmt.Fprintf(out, f+"\n", a...) }
	quiet := log.New(io.Discard, "", 0)
	gwLogs := &syncBuf{}

	model := httptest.NewServer((&fakemodel.Server{Models: []string{"qwen3-8b", "broken"}, Delay: 5 * time.Millisecond}).Handler())
	defer model.Close()
	key := bytes.Repeat([]byte{7}, 32) // demo-only key; production loads from KMS/env
	ciph, _ := store.NewCipher(key)
	dbPath := filepath.Join(dir, "gateway.db")
	st, err := store.OpenSQLite(dbPath, ciph)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	gw, err := gateway.New(gateway.Config{Store: st, Logger: log.New(gwLogs, "gateway ", log.LstdFlags), TokenTTL: 3 * time.Second, PingInterval: 500 * time.Millisecond})
	if err != nil {
		return nil, err
	}
	acmeKey, betaKey := strings.Repeat("a", 40), strings.Repeat("b", 40)
	_ = gw.RegisterTenant(ctx, "acme", "Acme", acmeKey)
	_ = gw.RegisterTenant(ctx, "beta", "Beta", betaKey)
	gws := httptest.NewServer(gw.Handler())
	defer gws.Close()

	acme := httptest.NewUnstartedServer(nil)
	acme.Config.Handler = saas.New(saas.Config{PublicURL: "http://" + acme.Listener.Addr().String(), GatewayURL: gws.URL, TenantID: "acme", TenantAPIKey: acmeKey,
		Users: map[string]string{"alice": "alice-pw", "bob": "bob-pw"}}).Handler()
	acme.Start()
	defer acme.Close()
	say("① 启动：假本机模型 %s，网关 %s（SQLite+加密），Mock SaaS(acme) %s", model.URL, gws.URL, acme.URL)

	browser := func(user, pw string) (*http.Client, error) {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		r, err := c.PostForm(acme.URL+"/login", url.Values{"user": {user}, "password": {pw}})
		if err != nil {
			return nil, err
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return nil, fmt.Errorf("login %s failed: %d", user, r.StatusCode)
		}
		return c, nil
	}
	alice, err := browser("alice", "alice-pw")
	if err != nil {
		return nil, err
	}
	bob, err := browser("bob", "bob-pw")
	if err != nil {
		return nil, err
	}

	pairing, err := client.StartPairing(ctx, nil, acme.URL, "Alice-MacBook")
	if err != nil {
		return nil, err
	}
	code := pairing.UserCode
	say("② 桌面连接器显示配对码 %s（打开 %s 输入）", code, pairing.LinkURL)
	req, _ := http.NewRequest("POST", acme.URL+"/link", strings.NewReader(url.Values{"user_code": {code}, "jkt": {pairing.JKT}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	r, err := alice.Do(req)
	if err != nil {
		return nil, err
	}
	var appr map[string]string
	_ = json.NewDecoder(r.Body).Decode(&appr)
	r.Body.Close()
	say("③ alice 在已登录页面输入配对码（链接携带设备公钥指纹 jkt=%s…，SaaS 记录为 E2EE 信任锚）→ %v", pairing.JKT[:10], appr)
	id, err := pairing.Wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("pair: %w", err)
	}
	if err := client.SaveIdentityFile(filepath.Join(dir, "identity.json"), id); err != nil {
		return nil, err
	}
	exec := &countingExec{OpenAIExecutor: &client.OpenAIExecutor{BaseURL: model.URL, ModelList: []string{"qwen3-8b", "broken"},
		Caps: map[string][]string{"qwen3-8b": {lsp2.ModelCapTools, lsp2.ModelCapVision}}}}
	conn, err := client.New(client.Config{Executor: exec, MaxConcurrency: 4, Logger: quiet}, id)
	if err != nil {
		return nil, err
	}
	res.DeviceID = conn.DeviceID()
	say("④ 设备换到短期令牌，身份已保存（私钥只在本机）：device_id=%s", res.DeviceID)

	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	ready := make(chan error, 4)
	go conn.Run(runCtx, ready)
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		return nil, errors.New("connector did not connect")
	}
	say("⑤ WebSocket(lsp2) 握手完成：hello 带设备签名 proof、X25519 密钥声明、flow 窗口；协商能力 resume/flow/tools/multimodal/e2ee，并发 4")

	get := func(c *http.Client, p string) (int, map[string]any) {
		r, err := c.Get(acme.URL + p)
		if err != nil {
			return 0, nil
		}
		defer r.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		return r.StatusCode, m
	}
	_, models := get(alice, "/api/models")
	b, _ := json.Marshal(models["data"])
	say("⑥ alice 可见模型：%s", b)

	var lastFinish string
	streamChat := func(c *http.Client, ctx context.Context, path string, payload map[string]any, onDelta func(string) bool) (string, string, string, error) {
		payload["stream"] = true
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, "POST", acme.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r, err := c.Do(req)
		if err != nil {
			return "", "", "", err
		}
		defer r.Body.Close()
		task := r.Header.Get("X-Task-Id")
		if r.StatusCode != 200 {
			var e struct {
				Error struct{ Code, Message string }
			}
			_ = json.NewDecoder(r.Body).Decode(&e)
			return "", task, e.Error.Code, nil
		}
		var sb strings.Builder
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			l := sc.Text()
			if !strings.HasPrefix(l, "data: ") {
				continue
			}
			p := strings.TrimPrefix(l, "data: ")
			if p == "[DONE]" {
				break
			}
			var ch struct {
				Choices []struct {
					Delta        struct{ Content string }
					FinishReason *string `json:"finish_reason"`
				}
				Usage map[string]int
				Error *struct{ Code, Message string }
			}
			_ = json.Unmarshal([]byte(p), &ch)
			if ch.Error != nil {
				return sb.String(), task, ch.Error.Code, nil
			}
			for _, x := range ch.Choices {
				sb.WriteString(x.Delta.Content)
				if onDelta != nil && x.Delta.Content != "" && !onDelta(x.Delta.Content) {
					return sb.String(), task, "", nil
				}
				if x.FinishReason != nil {
					lastFinish = fmt.Sprintf("finish_reason=%s usage=%v", *x.FinishReason, ch.Usage)
				}
			}
		}
		return sb.String(), task, "", nil
	}
	chat := func(c *http.Client, ctx context.Context, model, prompt string, onDelta func(string) bool) (string, string, string, error) {
		return streamChat(c, ctx, "/api/chat", map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": prompt}}}, onDelta)
	}
	postJSON := func(c *http.Client, path string, payload any) (int, map[string]any) {
		body, _ := json.Marshal(payload)
		r, err := c.Post(acme.URL+path, "application/json", bytes.NewReader(body))
		if err != nil {
			return 0, nil
		}
		defer r.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		return r.StatusCode, m
	}
	message := func(m map[string]any) map[string]any {
		ch, _ := m["choices"].([]any)
		if len(ch) == 0 {
			return map[string]any{}
		}
		msg, _ := ch[0].(map[string]any)["message"].(map[string]any)
		return msg
	}

	fmt.Fprint(out, "⑦ 流式对话：")
	text, _, ecode, err := chat(alice, ctx, res.DeviceID+"/qwen3-8b", "帮我写一句问候", func(d string) bool { fmt.Fprint(out, d); return true })
	fmt.Fprintf(out, "\n   └ %s\n", lastFinish)
	if err != nil || ecode != "" {
		return nil, fmt.Errorf("stream failed: %v %s", err, ecode)
	}
	res.StreamText = text

	devModel := res.DeviceID + "/qwen3-8b"

	// ── 断线续传 ──
	n0, drops := 0, 0
	text, _, ecode, err = chat(alice, ctx, devModel, "请给我一个长的回答", func(string) bool {
		n0++
		if n0 == 120 || n0 == 300 {
			drops++
			conn.Disconnect() // 模拟设备网络闪断（流的中途，两次）
		}
		return true
	})
	res.Resumes = gw.Resumes()
	res.ResumeLossless = err == nil && ecode == "" && text == strings.Repeat("这是一段很长的回答。", 50)
	say("⑦a 断线续传：流式输出途中设备 WebSocket 断开 %d 次 → 重连并 resume %d 次；收到 %d 字，与预期逐字一致（无丢失/无重复）：%v", drops, res.Resumes, len([]rune(text)), res.ResumeLossless)

	// ── 流量控制 / 背压 ──
	bpBody, _ := json.Marshal(map[string]any{"model": devModel, "stream": true, "messages": []map[string]string{{"role": "user", "content": "请给我一个超长的回答"}}})
	before := exec.n.Load()
	// 浏览器侧使用很小的 TCP 接收缓冲区，让“暂停读取”尽快传导回网关。
	slowReader := &http.Client{Jar: alice.Jar, Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(64 << 10)
		}
		return c, err
	}}}
	bpResp, err := slowReader.Post(acme.URL+"/api/chat", "application/json", bytes.NewReader(bpBody))
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(bpResp.Body, 256)
	_, _ = br.ReadString('\n')
	var stalled int64 = -1
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		time.Sleep(500 * time.Millisecond)
		cur := exec.n.Load() - before
		if cur == stalled {
			break
		}
		stalled = cur
	}
	res.BackpressureAt = stalled
	chunks := 0
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		chunks += strings.Count(sc.Text(), "流流流流流流流流流流")
		if sc.Text() == "data: [DONE]" {
			break
		}
	}
	bpResp.Body.Close()
	res.BackpressureAll = chunks / 9
	say("⑦b 流控背压：客户端暂停读取 → 网关停止 task.ack → 设备发送窗口(1 MiB)耗尽，执行器在第 %d/%d 个分片处阻塞；恢复读取后共收到 %d 个分片", res.BackpressureAt, fakemodel.VeryLongChunks, res.BackpressureAll)

	// ── tools 往返 ──
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather", "description": "查询天气", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]string{"type": "string"}}}}}}
	_, m1 := postJSON(alice, "/api/chat", map[string]any{"model": devModel, "tools": tools, "messages": []any{map[string]any{"role": "user", "content": "上海天气如何？"}}})
	msg1 := message(m1)
	calls, _ := msg1["tool_calls"].([]any)
	if len(calls) == 1 {
		fn := calls[0].(map[string]any)["function"].(map[string]any)
		res.ToolCall = fmt.Sprintf("%s(%s)", fn["name"], fn["arguments"])
		_, m2 := postJSON(alice, "/api/chat", map[string]any{"model": devModel, "tools": tools, "messages": []any{
			map[string]any{"role": "user", "content": "上海天气如何？"}, msg1,
			map[string]any{"role": "tool", "tool_call_id": calls[0].(map[string]any)["id"], "content": "晴，22°C"}}})
		res.ToolAnswer, _ = message(m2)["content"].(string)
	}
	say("⑦c tools 往返：模型流式返回 tool_call %s（finish_reason=tool_calls）→ 回传 tool 消息 → %q", res.ToolCall, res.ToolAnswer)

	// ── 图片输入（>16 KiB，走 blob 分块）──
	img := bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 30<<10) // 120 KiB
	_, m3 := postJSON(alice, "/api/chat", map[string]any{"model": devModel, "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "描述这张图"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)}}}}}})
	res.ImageReply, _ = message(m3)["content"].(string)
	if r := []rune(res.ImageReply); len(r) > 40 {
		res.ImageReply = string(r[:40]) + "…"
	}
	say("⑦d 图片输入：120 KiB data: URI 被网关移入 blob（task.run 签入 size+sha256，随后 2 个 ≤96 KiB 的 blob.chunk）→ 设备重组后交给模型：%q", res.ImageReply)

	// ── 端到端加密 ──
	secret := "机密：项目代号青鸾，预算 4200 万"
	e2Text, e2Task, e2Code, err := streamChat(alice, ctx, "/api/chat/e2ee", map[string]any{"model": devModel, "messages": []map[string]string{{"role": "user", "content": secret}}}, nil)
	if err != nil || e2Code != "" {
		return nil, fmt.Errorf("e2ee chat: %v %s", err, e2Code)
	}
	res.E2EEText = e2Text
	stTask, _ := st.GetTask(ctx, e2Task)
	evs, _ := st.ListEvents(ctx, e2Task)
	var dump bytes.Buffer
	dump.Write(stTask.Request)
	sealedOnly := len(evs) > 0
	for _, e := range evs {
		dump.Write(e.Data)
		if e.Kind == lsp2.TypeTaskDelta && !bytes.Contains(e.Data, []byte(`"e2ee"`)) {
			sealedOnly = false
		}
	}
	time.Sleep(200 * time.Millisecond)
	rawDB, _ := os.ReadFile(dbPath)
	walDB, _ := os.ReadFile(dbPath + "-wal")
	needles := [][]byte{[]byte("青鸾"), []byte("4200")}
	for _, n := range needles {
		res.E2EEPlainInStore = res.E2EEPlainInStore || bytes.Contains(dump.Bytes(), n) || bytes.Contains(rawDB, n) || bytes.Contains(walDB, n)
		res.E2EEPlainInLogs = res.E2EEPlainInLogs || strings.Contains(gwLogs.String(), string(n))
	}
	res.E2EECiphertext = sealedOnly
	say("⑦e 端到端加密：SaaS 用已钉住 jkt 校验设备 X25519 密钥声明 → JWE(ECDH-ES+X25519, A256GCM) 封装请求；设备解密执行并逐帧 AES-GCM 封装输出")
	say("    SaaS 解密得到：%q", e2Text)
	say("    网关存储（解密静态加密后）与日志中出现明文？存储=%v 日志=%v；网关仅转发密文帧：%v（任务 %s，%d 个事件）", res.E2EEPlainInStore, res.E2EEPlainInLogs, res.E2EECiphertext, e2Task, len(evs))

	cctx, ccancel := context.WithCancel(ctx)
	n := 0
	_, task, _, _ := chat(alice, cctx, "qwen3-8b", "请给我一个很长的回答", func(string) bool { n++; return n < 10 })
	ccancel()
	for i := 0; i < 50; i++ {
		t, _ := st.GetTask(ctx, task)
		res.CancelState = t.State
		if t.State != "RUNNING" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	say("⑧ 取消：收到 %d 个分片后浏览器断开 → 网关下发 task.cancel，任务 %s 状态=%s", n, task, res.CancelState)

	_, _, res.UpstreamErrCode, _ = chat(alice, ctx, "broken", "hi", nil)
	_, _, res.NotFoundCode, _ = chat(alice, ctx, "gpt-unknown", "hi", nil)
	say("⑨ 错误：本机模型崩溃 → %s；不存在的模型 → %s", res.UpstreamErrCode, res.NotFoundCode)

	_, bm := get(bob, "/api/models")
	bl, _ := bm["data"].([]any)
	res.BobModels = len(bl)
	rv, _ := http.NewRequest("POST", acme.URL+"/api/devices/revoke", strings.NewReader("device_id="+res.DeviceID))
	rv.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if r, err := bob.Do(rv); err == nil {
		res.BobRevokeStatus = r.StatusCode
		r.Body.Close()
	}
	say("⑩ 隔离：同租户的 bob 看到 %d 个模型，尝试吊销 alice 的设备 → HTTP %d", res.BobModels, res.BobRevokeStatus)

	for i := 0; i < 50 && gw.TokenRefreshes() < 1; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	res.TokenRefreshes = gw.TokenRefreshes()
	_, md := get(alice, "/api/models")
	ml, _ := md["data"].([]any)
	say("⑪ 会话内令牌续期（令牌有效期设为 3 秒演示）：已续期 %d 次，设备仍在线（可见模型 %d 个）", res.TokenRefreshes, len(ml))

	raw, _ := os.ReadFile(dbPath)
	wal, _ := os.ReadFile(dbPath + "-wal")
	res.PromptInDB = bytes.Contains(append(raw, wal...), []byte("帮我写一句问候"))
	say("⑫ 静态加密：数据库文件中能否找到明文提示词？%v", res.PromptInDB)

	rv, _ = http.NewRequest("POST", acme.URL+"/api/devices/revoke", strings.NewReader("device_id="+res.DeviceID))
	rv.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if r, err := alice.Do(rv); err == nil {
		r.Body.Close()
	}
	time.Sleep(300 * time.Millisecond)
	_, code2 := get(alice, "/api/devices")
	devs, _ := code2["devices"].([]any)
	if len(devs) == 1 {
		d := devs[0].(map[string]any)
		res.RevokedRefresh = d["status"] == "revoked" && d["online"] == false
	}
	say("⑬ alice 吊销设备 → 状态 revoked、会话被关闭、设备离线：%v", res.RevokedRefresh)
	return res, nil
}
