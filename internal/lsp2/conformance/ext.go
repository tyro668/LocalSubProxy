package conformance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"localsubproxy/internal/lsp2"
)

// Checks for the negotiated extensions: resume (X-R*), flow control (X-F*),
// tools and multimodal (X-T*, X-M*), end-to-end encryption (X-E*) and the
// HTTP re-attach/idempotency surface (X-H*).

type extDev struct {
	r      *runner
	conn   *websocket.Conn
	wmu    sync.Mutex
	mu     sync.Mutex
	recv   map[string]int64 // highest contiguous gateway seq per task
	gaps   []string
	frames chan lsp2.Envelope
}

func (d *extDev) write(e lsp2.Envelope) error {
	e.V = lsp2.Version
	d.wmu.Lock()
	defer d.wmu.Unlock()
	_ = d.conn.SetWriteDeadline(time.Now().Add(d.r.cfg.Timeout))
	return d.conn.WriteJSON(e)
}

func (d *extDev) send(typ, taskID string, seq int64, data any) error {
	b, _ := json.Marshal(data)
	return d.write(lsp2.Envelope{Type: typ, TaskID: taskID, Seq: seq, Data: b})
}

// pump reads frames, tracks gateway data-frame seqs and acks them (a device
// that negotiated flow MUST ack, otherwise the gateway stalls).
func (d *extDev) pump() {
	defer close(d.frames)
	for {
		f, err := d.r.readFrame(d.conn)
		if err != nil {
			return
		}
		if f.Seq > 0 && f.TaskID != "" {
			d.mu.Lock()
			if f.Seq == d.recv[f.TaskID]+1 {
				d.recv[f.TaskID] = f.Seq
			} else if f.Seq > d.recv[f.TaskID]+1 {
				d.gaps = append(d.gaps, fmt.Sprintf("%s: %d after %d", f.TaskID, f.Seq, d.recv[f.TaskID]))
			}
			seq := d.recv[f.TaskID]
			d.mu.Unlock()
			b, _ := json.Marshal(lsp2.TaskAck{Seq: seq})
			_ = d.write(lsp2.Envelope{Type: lsp2.TypeTaskAck, TaskID: f.TaskID, Data: b})
		}
		d.frames <- f
	}
}

func (d *extDev) await(typ string, pred func(lsp2.Envelope) bool) (lsp2.Envelope, []lsp2.Envelope, error) {
	var before []lsp2.Envelope
	tm := time.After(d.r.cfg.Timeout)
	for {
		select {
		case f, ok := <-d.frames:
			if !ok {
				return f, before, errors.New("session closed")
			}
			if f.Type == typ && (pred == nil || pred(f)) {
				return f, before, nil
			}
			before = append(before, f)
		case <-tm:
			return lsp2.Envelope{}, before, fmt.Errorf("timeout waiting for %s", typ)
		}
	}
}

// blobs collects the blob.chunk frames of run's task until every blob
// listed in the signed payload is complete.
func (d *extDev) blobs(run lsp2.TaskRun) []lsp2.Envelope {
	var out []lsp2.Envelope
	need := map[string]bool{}
	for _, b := range run.Blobs {
		need[b.ID] = true
	}
	for len(need) > 0 {
		f, _, err := d.await(lsp2.TypeBlobChunk, func(e lsp2.Envelope) bool { return e.TaskID == run.TaskID })
		if err != nil {
			return out
		}
		out = append(out, f)
		var c lsp2.BlobChunk
		_ = json.Unmarshal(f.Data, &c)
		if c.Last {
			delete(need, c.BlobID)
		}
	}
	return out
}

func (r *runner) dialExt(at string, hello lsp2.Hello) (*extDev, lsp2.Welcome, error) {
	var wel lsp2.Welcome
	wsURL := "ws" + strings.TrimPrefix(r.cfg.GatewayURL, "http") + "/v2/connect"
	d := websocket.Dialer{Subprotocols: []string{lsp2.Subprotocol}, HandshakeTimeout: r.cfg.Timeout}
	conn, resp, err := d.Dial(wsURL, http.Header{"Authorization": {"Bearer " + at}})
	if err != nil {
		return nil, wel, fmt.Errorf("dial: %v %s", err, status(resp))
	}
	dev := &extDev{r: r, conn: conn, recv: map[string]int64{}, frames: make(chan lsp2.Envelope, 256)}
	b, _ := json.Marshal(hello)
	if err := dev.write(lsp2.Envelope{Type: lsp2.TypeHello, ID: "h2", Data: b}); err != nil {
		return nil, wel, err
	}
	f, err := r.readFrame(conn)
	if err != nil || f.Type != lsp2.TypeWelcome {
		conn.Close()
		return nil, wel, fmt.Errorf("no welcome: %+v %v", f, err)
	}
	_ = json.Unmarshal(f.Data, &wel)
	go dev.pump()
	return dev, wel, nil
}

// chatRaw posts a chat request and returns status, headers and the body.
func (r *runner) chatRaw(ctx context.Context, body any, hdr map[string]string) (int, http.Header, []byte) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.GatewayURL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+r.cfg.TenantKey)
	req.Header.Set("X-End-User", r.cfg.EndUser)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0, nil, nil
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, resp.Header, buf.Bytes()
}

func errCode(b []byte) string {
	var m struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Error.Code
}

type sseEvent struct {
	id   string
	data string
}

func parseSSE(b []byte) []sseEvent {
	var out []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "id: "):
			cur.id = l[4:]
		case strings.HasPrefix(l, "data: "):
			cur.data = l[6:]
		case l == "":
			if cur.data != "" {
				out = append(out, cur)
			}
			cur = sseEvent{}
		}
	}
	if cur.data != "" {
		out = append(out, cur)
	}
	return out
}

func sseText(evs []sseEvent) string {
	var sb strings.Builder
	for _, e := range evs {
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(e.data), &c) == nil && len(c.Choices) > 0 {
			sb.WriteString(c.Choices[0].Delta.Content)
		}
	}
	return sb.String()
}

func (r *runner) runExt(ctx context.Context, key ed25519.PrivateKey, at string) (*websocket.Conn, chan lsp2.Envelope) {
	xpriv, xpub := lsp2.NewX25519()
	stmt := lsp2.SignE2EEKey(key, r.devID, xpub, time.Now())
	mTools, mPlain := "conf-vision-tools", "conf-plain"
	flow := &lsp2.Flow{TaskWindow: 256 << 10, ConnWindow: 1 << 20}
	hello := lsp2.Hello{DeviceID: r.devID, Client: "lsp2-conformance", Models: []string{mTools, mPlain}, MaxConcurrency: 4,
		ModelCapabilities: map[string][]string{mTools: {lsp2.ModelCapTools, lsp2.ModelCapVision}},
		Capabilities:      lsp2.Capabilities, Flow: flow, E2EEKey: stmt,
		Proof: lsp2.SignProof(key, r.devID, lsp2.ProofHello, at, time.Now())}
	dev, wel, err := r.dialExt(at, hello)
	if !r.check("X1", "hello offering resume/flow/tools/multimodal/e2ee is answered by welcome negotiating them, with resume_token and flow windows",
		want(err == nil && containsS(wel.Capabilities, lsp2.CapResume) && containsS(wel.Capabilities, lsp2.CapFlow) && containsS(wel.Capabilities, lsp2.CapTools) &&
			containsS(wel.Capabilities, lsp2.CapMultimodal) && containsS(wel.Capabilities, lsp2.CapE2EE) && wel.ResumeToken != "" && wel.ResumeSeconds > 0 &&
			wel.Flow != nil && wel.Flow.TaskWindow >= 65536 && wel.Flow.ConnWindow >= wel.Flow.TaskWindow, "err %v welcome %+v", err, wel)) {
		return nil, nil
	}
	full := func(m string) string { return r.devID + "/" + m }

	// devices / models surface
	st, dl, _, _ := r.tenant(http.MethodGet, "/v2/tenant/devices", nil)
	var dj map[string]any
	if ds, _ := dl["devices"].([]any); len(ds) > 0 {
		for _, x := range ds {
			if m, _ := x.(map[string]any); m["device_id"] == r.devID {
				dj = m
			}
		}
	}
	r.check("X-E1", "device list exposes jkt and the latest e2ee key statement", want(st == 200 && dj != nil && dj["jkt"] == lsp2.Thumbprint(key.Public().(ed25519.PublicKey)) && dj["e2ee_key"] == stmt, "status %d %v", st, dj))
	st, ml, _, _ := r.tenant(http.MethodGet, "/v1/models", nil)
	capsOK := false
	if data, _ := ml["data"].([]any); data != nil {
		for _, it := range data {
			if mm, _ := it.(map[string]any); mm["id"] == full(mTools) {
				cs, _ := mm["capabilities"].([]any)
				capsOK = len(cs) == 2
			}
		}
	}
	r.check("X-T1", "GET /v1/models reports per-model capabilities", want(st == 200 && capsOK, "status %d %v", st, ml))

	// tools round trip
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]string{"type": "string"}}}}}}
	type res struct {
		st   int
		h    http.Header
		body []byte
	}
	call := func(body any, hdr map[string]string) chan res {
		ch := make(chan res, 1)
		go func() { s, h, b := r.chatRaw(ctx, body, hdr); ch <- res{s, h, b} }()
		return ch
	}
	tc := call(map[string]any{"model": full(mTools), "tools": tools, "tool_choice": "auto", "messages": []any{map[string]any{"role": "user", "content": "weather?"}}}, nil)
	f, _, err := dev.await(lsp2.TypeTaskRun, nil)
	var run lsp2.TaskRun
	if err == nil {
		run, err = lsp2.VerifyTaskRun(r.gwKey, f.TaskID, f.JWS)
	}
	r.check("X-T2", "tools and tool_choice are carried in the signed task.run; task.run is a data frame with seq", want(err == nil && len(run.Tools) == 1 && run.Tools[0].Function.Name == "get_weather" && f.Seq >= 1, "err %v run %+v", err, run))
	if err == nil {
		idx := 0
		_ = dev.send(lsp2.TypeTaskDelta, run.TaskID, 1, lsp2.TaskDelta{ToolCalls: []lsp2.ToolCall{{Index: &idx, ID: "call_1", Type: "function", Function: lsp2.FunctionCall{Name: "get_weather", Arguments: `{"city":`}}}})
		_ = dev.send(lsp2.TypeTaskDelta, run.TaskID, 2, lsp2.TaskDelta{ToolCalls: []lsp2.ToolCall{{Index: &idx, Function: lsp2.FunctionCall{Arguments: `"Paris"}`}}}})
		_ = dev.send(lsp2.TypeTaskDone, run.TaskID, 3, lsp2.TaskDone{FinishReason: "tool_calls"})
		_, _, aerr := dev.await(lsp2.TypeTaskAck, func(e lsp2.Envelope) bool {
			var a lsp2.TaskAck
			_ = json.Unmarshal(e.Data, &a)
			return e.TaskID == run.TaskID && a.Seq >= 3
		})
		rs := <-tc
		var cc struct {
			Choices []struct {
				Message      lsp2.Message `json:"message"`
				FinishReason string       `json:"finish_reason"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(rs.body, &cc)
		ok := len(cc.Choices) == 1 && cc.Choices[0].FinishReason == "tool_calls" && len(cc.Choices[0].Message.ToolCalls) == 1 &&
			cc.Choices[0].Message.ToolCalls[0].Function.Arguments == `{"city":"Paris"}` && cc.Choices[0].Message.ToolCalls[0].ID == "call_1"
		r.check("X-T3", "streamed tool_call fragments are merged into message.tool_calls with finish_reason tool_calls", want(rs.st == 200 && ok, "status %d body %s", rs.st, rs.body))
		r.check("X-F1", "gateway acknowledges device data frames with task.ack (flow negotiated)", aerr)
	}
	s2, _, b2 := r.chatRaw(ctx, map[string]any{"model": full(mPlain), "tools": tools, "messages": []any{map[string]any{"role": "user", "content": "x"}}}, nil)
	r.check("X-T4", "tools sent to a model without the tools capability fail with 400 CAPABILITY_UNSUPPORTED", want(s2 == 400 && errCode(b2) == lsp2.ErrUnsupported, "status %d %s", s2, b2))
	r.runAnthropic(ctx, dev, full, mTools)

	// image → blob.chunk
	img := make([]byte, 40<<10)
	_, _ = rand.Read(img)
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)
	s3, _, b3 := r.chatRaw(ctx, map[string]any{"model": full(mPlain), "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}}}}}}, nil)
	r.check("X-M1", "image input to a model without vision fails with 400 CAPABILITY_UNSUPPORTED", want(s3 == 400 && errCode(b3) == lsp2.ErrUnsupported, "status %d %s", s3, b3))
	ic := call(map[string]any{"model": full(mTools), "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "what is this"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}}}}}}, nil)
	f, _, err = dev.await(lsp2.TypeTaskRun, nil)
	if err == nil {
		run, err = lsp2.VerifyTaskRun(r.gwKey, f.TaskID, f.JWS)
	}
	before := dev.blobs(run)
	var got []byte
	var seqs []int64
	for _, e := range before {
		if e.Type == lsp2.TypeBlobChunk && e.TaskID == run.TaskID {
			var c lsp2.BlobChunk
			_ = json.Unmarshal(e.Data, &c)
			p, _ := lsp2.UnB64(c.Data)
			got = append(got, p...)
			seqs = append(seqs, e.Seq)
		}
	}
	sum := sha256.Sum256(got)
	blobOK := err == nil && len(run.Blobs) == 1 && run.Blobs[0].Size == int64(len(img)) && run.Blobs[0].SHA256 == lsp2.B64(sum[:]) && bytes.Equal(got, img) &&
		len(run.Messages) == 1 && len(run.Messages[0].Content.Parts) == 2 && run.Messages[0].Content.Parts[1].ImageURL.URL == lsp2.BlobScheme+run.Blobs[0].ID
	r.check("X-M2", "data: URIs above 16 KiB travel as blob.chunk data frames after task.run; the signed blob ref (size, sha256) matches and the part references lsp2-blob:<id>", want(blobOK, "err %v run %+v blobs %d bytes", err, run.Blobs, len(got)))
	contig := len(seqs) > 0 && f.Seq == 1
	for i, s := range seqs {
		contig = contig && s == int64(i+2)
	}
	r.check("X-F2", "gateway data frames of a task are numbered 1..n without gaps (task.run = 1, then blob.chunk)", want(contig && len(dev.gaps) == 0, "run seq %d blob seqs %v gaps %v", f.Seq, seqs, dev.gaps))
	if err == nil {
		_ = dev.send(lsp2.TypeTaskDone, run.TaskID, 1, lsp2.TaskDone{FinishReason: "stop"})
		<-ic
	}

	// E2EE
	rkey := make([]byte, 32)
	_, _ = rand.Read(rkey)
	rid := lsp2.RandomID("rid_")
	plain, _ := json.Marshal(lsp2.E2EERequest{RID: rid, Iat: time.Now().Unix(), Model: mTools, Messages: []lsp2.Message{{Role: "user", Content: lsp2.Text("secret prompt")}}, ResponseKey: lsp2.B64(rkey)})
	jwe, _ := lsp2.SealJWE(xpub, lsp2.X25519Thumbprint(xpub), plain)
	s4, _, b4 := r.chatRaw(ctx, map[string]any{"model": full(mTools), "lsp2_e2ee": jwe, "messages": []any{map[string]any{"role": "user", "content": "leak"}}}, nil)
	r.check("X-E2", "lsp2_e2ee together with cleartext messages is rejected with 400", want(s4 == 400, "status %d %s", s4, b4))
	ec := call(map[string]any{"model": full(mTools), "stream": true, "lsp2_e2ee": jwe}, nil)
	f, _, err = dev.await(lsp2.TypeTaskRun, nil)
	if err == nil {
		run, err = lsp2.VerifyTaskRun(r.gwKey, f.TaskID, f.JWS)
	}
	before = dev.blobs(run)
	var sealed []byte
	for _, e := range before {
		if e.Type == lsp2.TypeBlobChunk && e.TaskID == run.TaskID {
			var c lsp2.BlobChunk
			_ = json.Unmarshal(e.Data, &c)
			p, _ := lsp2.UnB64(c.Data)
			sealed = append(sealed, p...)
		}
	}
	blind := err == nil && run.E2EE != nil && len(run.Messages) == 0 && len(run.Blobs) == 1 && run.Blobs[0].ID == run.E2EE.Blob && run.Blobs[0].MediaType == lsp2.E2EEMediaType && string(sealed) == jwe
	r.check("X-E3", "gateway relays the JWE unchanged as an application/jose blob referenced by task.run.e2ee and carries no cleartext messages", want(blind, "err %v run %+v", err, run))
	if err == nil {
		_, pt, oerr := lsp2.OpenJWE(xpriv, string(sealed))
		r.check("X-E4", "relayed JWE opens with the device X25519 key (ECDH-ES, A256GCM)", want(oerr == nil && bytes.Equal(pt, plain), "err %v", oerr))
		d1, _ := json.Marshal(lsp2.TaskDelta{Text: "top secret answer"})
		_ = dev.send(lsp2.TypeTaskDelta, run.TaskID, 1, lsp2.TaskDelta{E2EE: lsp2.SealChunk(rkey, rid, lsp2.TypeTaskDelta, 1, d1)})
		d2, _ := json.Marshal(map[string]string{"finish_reason": "stop"})
		_ = dev.send(lsp2.TypeTaskDone, run.TaskID, 2, lsp2.TaskDone{FinishReason: "stop", Usage: &lsp2.Usage{PromptTokens: 3, CompletionTokens: 3}, E2EE: lsp2.SealChunk(rkey, rid, lsp2.TypeTaskDone, 2, d2)})
		rs := <-ec
		var text string
		leak := bytes.Contains(rs.body, []byte("top secret"))
		opened := 0
		for _, e := range parseSSE(rs.body) {
			var c struct {
				E struct {
					Seq  int64  `json:"seq"`
					Type string `json:"type"`
					Data string `json:"data"`
				} `json:"lsp2_e2ee"`
			}
			if json.Unmarshal([]byte(e.data), &c) == nil && c.E.Data != "" {
				p, err := lsp2.OpenChunk(rkey, rid, c.E.Type, c.E.Seq, c.E.Data)
				if err == nil {
					opened++
					var d lsp2.TaskDelta
					_ = json.Unmarshal(p, &d)
					text += d.Text
				}
			}
		}
		r.check("X-E5", "SSE carries sealed frames as lsp2_e2ee {seq,type,data} that open with the response key; no plaintext in the stream", want(rs.st == 200 && !leak && opened == 2 && text == "top secret answer", "status %d opened %d leak %v body %s", rs.st, opened, leak, rs.body))
	}

	// idempotency
	key1 := lsp2.RandomID("idem_")
	ic2 := call(map[string]any{"model": full(mTools), "stream": true, "messages": []any{map[string]any{"role": "user", "content": "resume me"}}}, map[string]string{"Idempotency-Key": key1})
	f, _, err = dev.await(lsp2.TypeTaskRun, nil)
	if err == nil {
		run, err = lsp2.VerifyTaskRun(r.gwKey, f.TaskID, f.JWS)
	}
	s5, _, b5 := r.chatRaw(ctx, map[string]any{"model": full(mTools), "stream": true, "messages": []any{map[string]any{"role": "user", "content": "different"}}}, map[string]string{"Idempotency-Key": key1})
	r.check("X-H1", "same Idempotency-Key with a different body fails with 422 IDEMPOTENCY_KEY_REUSED", want(s5 == 422 && errCode(b5) == lsp2.ErrIdempotency, "status %d %s", s5, b5))
	if err != nil {
		r.check("X-R1", "resume", err)
		return dev.conn, dev.frames
	}
	_ = dev.send(lsp2.TypeTaskDelta, run.TaskID, 1, lsp2.TaskDelta{Text: "A"})
	_, _, _ = dev.await(lsp2.TypeTaskAck, func(e lsp2.Envelope) bool {
		return e.TaskID == run.TaskID && strings.Contains(string(e.Data), `"seq":1`)
	})

	// resume: drop the connection mid-task and reconnect within the window
	dev.mu.Lock()
	gwSeq := dev.recv[run.TaskID]
	dev.mu.Unlock()
	dev.conn.Close()
	for range dev.frames {
	}
	hello.Resume = &lsp2.HelloResume{Token: wel.ResumeToken, Tasks: []lsp2.ResumeTask{{TaskID: run.TaskID, RecvSeq: gwSeq}}}
	hello.Proof = lsp2.SignProof(key, r.devID, lsp2.ProofHello, at, time.Now())
	dev2, wel2, err := r.dialExt(at, hello)
	var rt lsp2.ResumeTask
	if wel2.Resume != nil && len(wel2.Resume.Tasks) == 1 {
		rt = wel2.Resume.Tasks[0]
	}
	if !r.check("X-R1", "hello.resume within the window is answered with welcome.resume {resumed:true, tasks:[{task_id, recv_seq}]} reporting the last device frame received",
		want(err == nil && wel2.Resume != nil && wel2.Resume.Resumed && rt.TaskID == run.TaskID && rt.RecvSeq == 1, "err %v welcome %+v", err, wel2)) {
		return nil, nil
	}
	dev2.mu.Lock()
	dev2.recv[run.TaskID] = gwSeq
	dev2.mu.Unlock()
	_ = dev2.send(lsp2.TypeTaskDelta, run.TaskID, 1, lsp2.TaskDelta{Text: "A"}) // duplicate retransmit: MUST be dropped
	_ = dev2.send(lsp2.TypeTaskDelta, run.TaskID, 2, lsp2.TaskDelta{Text: "B"})
	_ = dev2.send(lsp2.TypeTaskDone, run.TaskID, 3, lsp2.TaskDone{FinishReason: "stop"})
	rs := <-ic2
	evs := parseSSE(rs.body)
	r.check("X-R2", "after resume, retransmitted duplicates are dropped and the client sees each delta exactly once", want(rs.st == 200 && sseText(evs) == "AB" && evs[len(evs)-1].data == "[DONE]", "status %d text %q", rs.st, sseText(evs)))
	ids := true
	var firstContent string
	for _, e := range evs[:len(evs)-1] {
		ids = ids && e.id != ""
		if firstContent == "" && e.id != "" && e.id != "0" {
			firstContent = e.id
		}
	}
	r.check("X-H2", "every SSE event carries an id (role preamble id 0, then the data-frame seq)", want(len(evs) > 1 && ids, "events %+v", evs))
	// re-attach with Last-Event-ID
	if len(evs) > 2 {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.GatewayURL+"/v2/tenant/tasks/"+run.TaskID+"/events", nil)
		req.Header.Set("Authorization", "Bearer "+r.cfg.TenantKey)
		req.Header.Set("X-End-User", r.cfg.EndUser)
		req.Header.Set("Last-Event-ID", firstContent)
		resp, err := (&http.Client{Timeout: r.cfg.Timeout}).Do(req)
		var after []sseEvent
		if err == nil {
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(resp.Body)
			resp.Body.Close()
			after = parseSSE(buf.Bytes())
		}
		r.check("X-H3", "GET /v2/tenant/tasks/{id}/events with Last-Event-ID replays exactly the later events", want(err == nil && sseText(after) == "B" && len(after) > 0 && after[len(after)-1].data == "[DONE]", "err %v events %+v", err, after))
		s6, _, b6 := r.chatRaw(ctx, map[string]any{"model": full(mTools), "stream": true, "messages": []any{map[string]any{"role": "user", "content": "resume me"}}}, map[string]string{"Idempotency-Key": key1, "Last-Event-ID": firstContent})
		r.check("X-H4", "replaying the same Idempotency-Key and body re-attaches to the task instead of running it again", want(s6 == 200 && sseText(parseSSE(b6)) == "B", "status %d %s", s6, b6))
	}
	// sequence gap
	gc := call(map[string]any{"model": full(mTools), "stream": true, "messages": []any{map[string]any{"role": "user", "content": "gap"}}}, nil)
	f, _, err = dev2.await(lsp2.TypeTaskRun, nil)
	if err == nil {
		_ = dev2.send(lsp2.TypeTaskDelta, f.TaskID, 2, lsp2.TaskDelta{Text: "skipped one"})
		rs := <-gc
		r.check("X-F3", "a data frame that skips a seq fails the task (no silent loss)", want(!bytes.Contains(rs.body, []byte("skipped one")) && bytes.Contains(rs.body, []byte(`"error"`)), "body %s", rs.body))
	}
	return dev2.conn, dev2.frames
}
