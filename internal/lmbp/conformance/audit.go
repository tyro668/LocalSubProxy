package conformance

// Z*: regression cases from the 2026-10 security/robustness audit. Each check
// pins one finding as black-box behaviour every implementation must share.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

func (r *runner) runAudit(ctx context.Context, dev *extDev, full func(string) string, mTools string) {
	r.auditIdempotency(ctx, dev, full(mTools))
	r.auditResumeBounds(ctx, dev, full(mTools))
	r.auditAnthropicInterleave(ctx, dev, full(mTools))
}

// anthropicLifecycle checks the strict Messages streaming lifecycle:
// message_start first; blocks are started with consecutive indices 0..n-1,
// at most one open at a time; deltas and stops only for the open block;
// message_delta after every block is stopped; message_stop last. It returns
// the reconstructed text and tool inputs (name → concatenated partial_json).
func anthropicLifecycle(evs []namedEvent) (string, map[string]string, error) {
	text, tools := "", map[string]string{}
	open, next := -1, 0
	names := map[int]string{}
	if len(evs) == 0 || evs[0].event != "message_start" {
		return "", nil, errors.New("first event is not message_start")
	}
	for i, e := range evs[1:] {
		idx := -1
		if f, ok := e.data["index"].(float64); ok {
			idx = int(f)
		}
		switch e.event {
		case "content_block_start":
			if open != -1 {
				return "", nil, fmt.Errorf("event %d: block %d started while block %d is open", i+1, idx, open)
			}
			if idx != next {
				return "", nil, fmt.Errorf("event %d: block index %d, want %d", i+1, idx, next)
			}
			open, next = idx, next+1
			if cb, _ := e.data["content_block"].(map[string]any); cb["type"] == "tool_use" {
				n, _ := cb["name"].(string)
				names[idx] = n
				tools[n] += ""
			}
		case "content_block_delta":
			if idx != open {
				return "", nil, fmt.Errorf("event %d: delta for block %d but open block is %d", i+1, idx, open)
			}
			d, _ := e.data["delta"].(map[string]any)
			switch d["type"] {
			case "text_delta":
				t, _ := d["text"].(string)
				text += t
			case "input_json_delta":
				pj, _ := d["partial_json"].(string)
				tools[names[idx]] += pj
			}
		case "content_block_stop":
			if idx != open {
				return "", nil, fmt.Errorf("event %d: stop for block %d but open block is %d", i+1, idx, open)
			}
			open = -1
		case "message_delta":
			if open != -1 {
				return "", nil, fmt.Errorf("event %d: message_delta while block %d is open", i+1, open)
			}
		case "message_stop":
			if i+1 != len(evs)-1 {
				return "", nil, errors.New("message_stop is not the last event")
			}
		}
	}
	if evs[len(evs)-1].event != "message_stop" {
		return "", nil, errors.New("stream does not end with message_stop")
	}
	return text, tools, nil
}

// Z5: interleaved OpenAI-style tool_call deltas (and text between them) are
// converted into a valid Anthropic event lifecycle without losing any
// argument bytes (audit finding 5).
func (r *runner) auditAnthropicInterleave(ctx context.Context, dev *extDev, model string) {
	ix := func(i int) *int { return &i }
	body := map[string]any{"model": model, "stream": true, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "interleave"}},
		"tools": []any{map[string]any{"name": "fa", "input_schema": map[string]any{"type": "object"}}, map[string]any{"name": "fb", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "fc", "input_schema": map[string]any{"type": "object"}}}}
	ch := make(chan []byte, 1)
	go func() { _, _, b := r.messagesRaw(ctx, body, nil); ch <- b }()
	f, _, err := dev.await(lmbp.TypeTaskRun, nil)
	if err != nil {
		r.check("Z5", "anthropic interleaved tools", err)
		return
	}
	tc := func(i int, id, name, args string) lmbp.ToolCall {
		return lmbp.ToolCall{Index: ix(i), ID: id, Type: map[bool]string{true: "function"}[id != ""], Function: lmbp.FunctionCall{Name: name, Arguments: args}}
	}
	deltas := []lmbp.TaskDelta{
		{Text: "Hi"},
		{ToolCalls: []lmbp.ToolCall{tc(0, "call_a", "fa", `{"a"`)}},
		{ToolCalls: []lmbp.ToolCall{tc(1, "call_b", "fb", `{"b"`)}},
		{ToolCalls: []lmbp.ToolCall{tc(0, "", "", `:1}`)}},
		{Text: " there"},
		{ToolCalls: []lmbp.ToolCall{tc(2, "call_c", "fc", `{}`), tc(1, "", "", `:2}`)}},
	}
	for i, d := range deltas {
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, int64(i+1), d)
	}
	_ = dev.send(lmbp.TypeTaskDone, f.TaskID, int64(len(deltas)+1), lmbp.TaskDone{FinishReason: "tool_calls"})
	b := <-ch
	text, tools, lerr := anthropicLifecycle(parseNamedSSE(b))
	r.check("Z5", "interleaved tool_call deltas for three tools plus text produce a valid Anthropic lifecycle (one open block, no delta after content_block_stop) with all arguments intact",
		want(lerr == nil && text == "Hi there" && tools["fa"] == `{"a":1}` && tools["fb"] == `{"b":2}` && tools["fc"] == `{}`,
			"lifecycle %v text %q tools %v\n%s", lerr, text, tools, b))
}

// Z3: re-attaching to a finished stream with Last-Event-ID below, equal to
// or beyond the terminal event's seq always ends: below replays the rest,
// equal and beyond end promptly with no further content (audit finding 3).
// Covered on both /v1/chat/completions and /v1/messages.
func (r *runner) auditResumeBounds(ctx context.Context, dev *extDev, model string) {
	for _, api := range []string{"chat", "messages"} {
		do := func(c context.Context, hdr map[string]string) (int, []byte) {
			body := map[string]any{"model": model, "stream": true, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "resume bounds " + api}}}
			if api == "chat" {
				st, _, b := r.chatRaw(c, body, hdr)
				return st, b
			}
			st, _, b := r.messagesRaw(c, body, hdr)
			return st, b
		}
		key := lmbp.RandomID("idem_")
		ch := make(chan []byte, 1)
		go func() { _, b := do(ctx, map[string]string{"Idempotency-Key": key}); ch <- b }()
		f, _, err := dev.await(lmbp.TypeTaskRun, nil)
		if err != nil {
			r.check("Z3", "resume bounds "+api, err)
			return
		}
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 1, lmbp.TaskDelta{Text: "A"})
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 2, lmbp.TaskDelta{Text: "B"})
		_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 3, lmbp.TaskDone{FinishReason: "stop"})
		<-ch
		text := func(b []byte) string {
			if api == "chat" {
				return sseText(parseSSE(b))
			}
			var s string
			for _, e := range parseNamedSSE(b) {
				if d, _ := e.data["delta"].(map[string]any); e.event == "content_block_delta" && d != nil {
					t, _ := d["text"].(string)
					s += t
				}
			}
			return s
		}
		for _, c := range []struct {
			id, want, name string
		}{{"1", "B", "below"}, {"3", "", "equal to"}, {"99", "", "beyond"}} {
			cc, cancel := context.WithTimeout(ctx, 5*time.Second)
			start := time.Now()
			st, b := do(cc, map[string]string{"Idempotency-Key": key, "Last-Event-ID": c.id})
			hung := cc.Err() != nil
			cancel()
			r.check("Z3", fmt.Sprintf("/%s re-attach with Last-Event-ID %s the terminal seq (3) ends the stream (got %q)", api, c.name, c.want),
				want(!hung && st == 200 && text(b) == c.want, "status %d hung %v after %v body %q", st, hung, time.Since(start).Round(time.Millisecond), b))
		}
	}
}

// Z1: idempotency records are scoped to (tenant, end user, key) without any
// ambiguous concatenation: (user U, key "x|y") and (user "U|x", key "y") are
// different records, and a replay never reattaches another user's task.
func (r *runner) auditIdempotency(ctx context.Context, dev *extDev, model string) {
	body := map[string]any{"model": model, "stream": false, "messages": []any{map[string]any{"role": "user", "content": "idem"}}}
	type res struct {
		st   int
		body []byte
	}
	ch := make(chan res, 1)
	go func() {
		s, _, b := r.chatRaw(ctx, body, map[string]string{"Idempotency-Key": "x|y"})
		ch <- res{s, b}
	}()
	f, _, err := dev.await(lmbp.TypeTaskRun, nil)
	if err != nil {
		r.check("Z1", "idempotency key scoping", err)
		return
	}
	_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 1, lmbp.TaskDelta{Text: "owner-secret"})
	_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 2, lmbp.TaskDone{FinishReason: "stop"})
	first := <-ch
	st, _, b := r.chatRaw(ctx, body, map[string]string{"Idempotency-Key": "y", "X-End-User": r.cfg.EndUser + "|x"})
	r.check("Z1", "(user U, Idempotency-Key \"x|y\") and (user \"U|x\", key \"y\") do not collide: the second user cannot replay the first user's task",
		want(first.st == 200 && strings.Contains(string(first.body), "owner-secret") && st != 200 && !strings.Contains(string(b), "owner-secret"),
			"first %d; second %d %s", first.st, st, b))
	st, _, b = r.chatRaw(ctx, body, map[string]string{"Idempotency-Key": "x|y"})
	r.check("Z1b", "the owner's replay with the same key and body still returns the original task",
		want(st == 200 && strings.Contains(string(b), "owner-secret"), "status %d %s", st, b))
}

// Z2: the gateway never has more unacknowledged bytes in flight than the
// device's advertised windows, also when several tasks compete for the
// connection window and wake together on an ack (audit finding 2). The
// device reconnects with task = conn window = 262,144 bytes (>= max frame),
// acks in 30ms batches, and receives four concurrent 512 KiB image uploads as blob.chunk
// frames. It returns the new session for the remaining checks.
func (r *runner) auditFlow(ctx context.Context, key ed25519.PrivateKey, at string, hello lmbp.Hello, model string) *extDev {
	const window = 262144
	hello.Flow = &lmbp.Flow{TaskWindow: window, ConnWindow: window}
	hello.Proof = lmbp.SignProof(key, r.devID, lmbp.ProofHello, at, time.Now())
	dev, _, err := r.dialExt(at, hello)
	if !r.check("Z2", "flow windows hold under concurrency", err) {
		return nil
	}
	dev.mu.Lock()
	dev.batchAcks, dev.inflight = true, map[string]int{}
	dev.mu.Unlock()
	stop := make(chan struct{})
	go dev.ackLoop(stop)
	const n = 4
	img := make([]byte, 512<<10)
	_, _ = rand.Read(img)
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)
	sts := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			st, _, _ := r.chatRaw(ctx, map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": fmt.Sprint("flow ", i)}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}}}}}}, nil)
			sts <- st
		}()
	}
	runs, open, got := map[string]bool{}, map[string]bool{}, 0
	tm := time.After(r.cfg.Timeout)
	for err == nil && (len(runs) < n || len(open) > 0) {
		select {
		case f, ok := <-dev.frames:
			if !ok {
				err = fmt.Errorf("session closed")
				break
			}
			switch f.Type {
			case lmbp.TypeTaskRun:
				run, verr := lmbp.VerifyTaskRun(r.gwKey, f.TaskID, f.JWS)
				if verr != nil {
					err = verr
					break
				}
				runs[f.TaskID] = true
				for _, b := range run.Blobs {
					open[f.TaskID+"/"+b.ID] = true
				}
			case lmbp.TypeBlobChunk:
				var c lmbp.BlobChunk
				_ = json.Unmarshal(f.Data, &c)
				p, _ := lmbp.UnB64(c.Data)
				got += len(p)
				if c.Last {
					delete(open, f.TaskID+"/"+c.BlobID)
				}
			}
		case <-tm:
			err = fmt.Errorf("timeout: %d runs, %d blobs open, %d bytes", len(runs), len(open), got)
		}
	}
	for id := range runs {
		_ = dev.send(lmbp.TypeTaskDone, id, 1, lmbp.TaskDone{FinishReason: "stop"})
	}
	ok := 0
	for i := 0; i < len(runs); i++ {
		if <-sts == 200 {
			ok++
		}
	}
	close(stop)
	dev.mu.Lock()
	pt, pc := dev.peakTask, dev.peakConn
	dev.batchAcks = false
	dev.mu.Unlock()
	r.check("Z2", fmt.Sprintf("with %d tasks competing for a %d-byte window, unacknowledged gateway bytes never exceed the task or connection window", n, window),
		want(err == nil && ok == n && got == n*len(img) && pt <= window && pc <= window,
			"err %v ok %d/%d blob bytes %d; peak unacked per task %d, per connection %d (window %d)", err, ok, n, got, pt, pc, window))
	return dev
}

// joseMutations are header/claim mutations of a valid token that every
// verifier MUST reject (spec §5.2; spec/testvectors/jose.json has the full
// offline set). Applied black-box to client assertions and hello proofs (Z4).
func joseMutations(hdr, claims map[string]any, other ed25519.PublicKey) map[string][2]map[string]any {
	cp := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	h := func(f func(map[string]any)) [2]map[string]any {
		x := cp(hdr)
		f(x)
		return [2]map[string]any{x, claims}
	}
	c := func(f func(map[string]any)) [2]map[string]any {
		x := cp(claims)
		f(x)
		return [2]map[string]any{hdr, x}
	}
	return map[string][2]map[string]any{
		"b64:false":      h(func(x map[string]any) { x["b64"] = false }),
		"crit":           h(func(x map[string]any) { x["crit"] = []string{"b64"}; x["b64"] = false }),
		"unknown header": h(func(x map[string]any) { x["x-extra"] = "1" }),
		"kid mismatch":   h(func(x map[string]any) { x["kid"] = "other-key-" + lmbp.B64(other) }),
		"iat missing":    c(func(x map[string]any) { delete(x, "iat") }),
		"iat string":     c(func(x map[string]any) { x["iat"] = fmt.Sprint(x["iat"]) }),
		"iat fractional": c(func(x map[string]any) { x["iat"] = float64(x["iat"].(int64)) + 0.5 }),
		"jti missing":    c(func(x map[string]any) { delete(x, "jti") }),
		"jti wrong type": c(func(x map[string]any) { x["jti"] = 42 }),
	}
}

func signRawJWS(key ed25519.PrivateKey, hdr, claims map[string]any) string {
	hb, _ := json.Marshal(hdr)
	cb, _ := json.Marshal(claims)
	in := lmbp.B64(hb) + "." + lmbp.B64(cb)
	return in + "." + lmbp.B64(ed25519.Sign(key, []byte(in)))
}

// auditAssertions: Z4a — malformed client assertions get invalid_grant.
func (r *runner) auditAssertions(key ed25519.PrivateKey, refresh func(string) (int, map[string]any)) {
	pub := key.Public().(ed25519.PublicKey)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().Unix()
	base := func() map[string]any {
		return map[string]any{"iss": r.devID, "sub": r.devID, "aud": lmbp.AssertionAudience(r.issuer), "iat": now, "exp": now + 60, "jti": lmbp.RandomID("a_")}
	}
	var bad []string
	hdr := map[string]any{"alg": "EdDSA", "typ": lmbp.TypAssertion, "kid": lmbp.Thumbprint(pub)}
	for name := range joseMutations(hdr, base(), nil) {
		m := joseMutations(hdr, base(), otherKey.Public().(ed25519.PublicKey))[name] // fresh jti per attempt
		if st, _ := refresh(signRawJWS(key, m[0], m[1])); st == 200 {
			bad = append(bad, name)
		}
	}
	r.check("Z4a", "client assertions with b64:false, crit, unknown headers, kid mismatch or missing/mistyped iat/jti are rejected",
		want(len(bad) == 0, "accepted: %v", bad))
}

// auditHelloProofs: Z4b — hello with a malformed proof gets no welcome.
func (r *runner) auditHelloProofs(key ed25519.PrivateKey, at string) {
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	base := func() map[string]any {
		return map[string]any{"iss": r.devID, "aud": lmbp.ProofHello, "iat": time.Now().Unix(), "jti": lmbp.RandomID("p_"), "ath": lmbp.Ath(at)}
	}
	var bad []string
	d := websocket.Dialer{Subprotocols: []string{lmbp.Subprotocol}, HandshakeTimeout: r.cfg.Timeout}
	wsURL := "ws" + strings.TrimPrefix(r.cfg.GatewayURL, "http") + "/lmbp/v1/connect"
	hdr := map[string]any{"alg": "EdDSA", "typ": lmbp.TypProof}
	for name := range joseMutations(hdr, base(), nil) {
		m := joseMutations(hdr, base(), otherKey.Public().(ed25519.PublicKey))[name]
		conn, _, err := d.Dial(wsURL, http.Header{"Authorization": {"Bearer " + at}})
		if err != nil {
			bad = append(bad, name+" (dial: "+err.Error()+")")
			continue
		}
		_ = r.writeFrame(conn, "hello", "hz", "", lmbp.Hello{DeviceID: r.devID, Client: "lmbp-conformance", Models: []string{"m"}, MaxConcurrency: 1, Proof: signRawJWS(key, m[0], m[1])})
		if f, _ := r.readFrame(conn); f.Type == lmbp.TypeWelcome {
			bad = append(bad, name)
		}
		conn.Close()
	}
	r.check("Z4b", "hello proofs with b64:false, crit, unknown headers, a kid not matching the device key or missing/mistyped iat/jti get no welcome",
		want(len(bad) == 0, "accepted: %v", bad))
}
