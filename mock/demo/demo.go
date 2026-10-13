// Package demo wires fake model + gateway + two tenants' SaaS + connector
// in-process and walks through pair → connect → stream → cancel → errors →
// isolation → encryption-at-rest → revoke. Used by `lspmock demo` and tests.
package demo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"localsubproxy/mock/connector"
	"localsubproxy/mock/fakemodel"
	"localsubproxy/mock/gateway"
	"localsubproxy/mock/saas"
	"localsubproxy/mock/store"
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
}

func Run(ctx context.Context, out io.Writer, dir string) (*Result, error) {
	res := &Result{}
	say := func(f string, a ...any) { fmt.Fprintf(out, f+"\n", a...) }
	quiet := log.New(io.Discard, "", 0)

	model := httptest.NewServer((&fakemodel.Server{Models: []string{"qwen3-8b", "broken"}, Delay: 15 * time.Millisecond}).Handler())
	defer model.Close()
	key := bytes.Repeat([]byte{7}, 32) // demo-only key; production loads from KMS/env
	ciph, _ := store.NewCipher(key)
	dbPath := filepath.Join(dir, "gateway.db")
	st, err := store.OpenSQLite(dbPath, ciph)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	gw, err := gateway.New(gateway.Config{Store: st, Logger: quiet})
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

	codeCh := make(chan string, 1)
	conn := connector.New(connector.Config{SaaSURL: acme.URL, IdentityFile: filepath.Join(dir, "identity.json"), DeviceName: "Alice-MacBook",
		ModelURL: model.URL, Models: []string{"qwen3-8b", "broken"}, MaxConcurrency: 2, Logger: quiet,
		OnUserCode: func(code, link string) {
			say("② 桌面连接器显示配对码 %s（打开 %s 输入）", code, link)
			codeCh <- code
		}})
	pairErr := make(chan error, 1)
	go func() { pairErr <- conn.Pair(ctx) }()
	code := <-codeCh
	req, _ := http.NewRequest("POST", acme.URL+"/link", strings.NewReader(url.Values{"user_code": {code}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	r, err := alice.Do(req)
	if err != nil {
		return nil, err
	}
	var appr map[string]string
	_ = json.NewDecoder(r.Body).Decode(&appr)
	r.Body.Close()
	say("③ alice 在已登录页面输入配对码 → %v", appr)
	if err := <-pairErr; err != nil {
		return nil, fmt.Errorf("pair: %w", err)
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
	say("⑤ WebSocket(lsp2) 握手完成（hello 携带设备私钥签名的 proof）")

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

	chat := func(c *http.Client, ctx context.Context, model, prompt string, onDelta func(string) bool) (string, string, string, error) {
		body, _ := json.Marshal(map[string]any{"model": model, "stream": true, "messages": []map[string]string{{"role": "user", "content": prompt}}})
		req, _ := http.NewRequestWithContext(ctx, "POST", acme.URL+"/api/chat", bytes.NewReader(body))
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
					fmt.Fprintf(out, "\n   └ finish_reason=%s usage=%v", *x.FinishReason, ch.Usage)
				}
			}
		}
		return sb.String(), task, "", nil
	}

	fmt.Fprint(out, "⑦ 流式对话：")
	text, _, ecode, err := chat(alice, ctx, res.DeviceID+"/qwen3-8b", "帮我写一句问候", func(d string) bool { fmt.Fprint(out, d); return true })
	fmt.Fprintln(out)
	if err != nil || ecode != "" {
		return nil, fmt.Errorf("stream failed: %v %s", err, ecode)
	}
	res.StreamText = text

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

	raw, _ := os.ReadFile(dbPath)
	wal, _ := os.ReadFile(dbPath + "-wal")
	res.PromptInDB = bytes.Contains(append(raw, wal...), []byte("帮我写一句问候"))
	say("⑪ 静态加密：数据库文件中能否找到明文提示词？%v", res.PromptInDB)

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
	say("⑫ alice 吊销设备 → 状态 revoked、会话被关闭、设备离线：%v", res.RevokedRefresh)
	return res, nil
}
