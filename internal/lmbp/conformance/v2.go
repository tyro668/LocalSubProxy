package conformance

// LMBP 2.0 checks: version refusal (W8, W9), hello / welcome /
// models.update rules (W10-W13), post-handshake frames (F2), task.run seq
// (R3), metadata profiles (M8), length units (L1-L3) and the cancel / ACK /
// window-reclaim state table (S1-S3, spec §8.6).

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tyro668/avora/internal/lmbp"
)

func (r *runner) wsURL() string {
	return "ws" + strings.TrimPrefix(r.cfg.GatewayURL, "http") + "/lmbp/v1/connect"
}

// rawHello opens a session offering subprotocol 2.0, sends hello (with v as
// given and data as given) and returns the first frame the gateway sends.
func (r *runner) rawHello(at string, v int, data map[string]any) (lmbp.Envelope, error) {
	f, _, err := r.rawHelloThen(at, v, data)
	return f, err
}

// rawHelloThen also reports whether the gateway closed the connection after
// its first reply (a read that ends in a close or EOF, not a timeout).
func (r *runner) rawHelloThen(at string, v int, data map[string]any) (lmbp.Envelope, bool, error) {
	d := websocket.Dialer{Subprotocols: []string{lmbp.Subprotocol}, HandshakeTimeout: r.cfg.Timeout}
	conn, resp, err := d.Dial(r.wsURL(), http.Header{"Authorization": {"Bearer " + at}})
	if err != nil {
		return lmbp.Envelope{}, false, fmt.Errorf("dial: %v %s", err, status(resp))
	}
	defer conn.Close()
	b, _ := json.Marshal(data)
	_ = conn.SetWriteDeadline(time.Now().Add(r.cfg.Timeout))
	if err := conn.WriteJSON(map[string]any{"v": v, "type": "hello", "id": "hv", "data": json.RawMessage(b)}); err != nil {
		return lmbp.Envelope{}, false, err
	}
	f, err := r.readFrame(conn)
	if err != nil {
		return f, false, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, rerr := conn.ReadMessage()
	var ne net.Error
	closed := rerr != nil && !(errors.As(rerr, &ne) && ne.Timeout())
	return f, closed, nil
}

func errBody(f lmbp.Envelope) lmbp.ErrorBody {
	var e lmbp.ErrorBody
	_ = json.Unmarshal(f.Data, &e)
	return e
}

// runV2Handshake runs before the main session is established (it opens and
// closes its own sessions for the device).
func (r *runner) runV2Handshake(key ed25519.PrivateKey, at string) {
	// W8: the subprotocol carries no version; a versioned name (lmbp.2) without
	// "lmbp" is refused before the upgrade like any other missing subprotocol.
	d := websocket.Dialer{Subprotocols: []string{lmbp.Subprotocol + ".2"}, HandshakeTimeout: r.cfg.Timeout}
	_, resp, err := d.Dial(r.wsURL(), http.Header{"Authorization": {"Bearer " + at}})
	code, st := "", 0
	if resp != nil {
		st = resp.StatusCode
		var p map[string]any
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = json.Unmarshal(b, &p)
		code, _ = p["code"].(string)
	}
	r.check("W8", "an upgrade offering only lmbp.2 (no lmbp) is refused with 400 problem code VERSION_UNSUPPORTED",
		want(err != nil && st == 400 && code == lmbp.ErrVersion, "err %v status %d code %q", err, st, code))

	hello := func(mut func(map[string]any)) map[string]any {
		h := map[string]any{"device_id": r.devID, "client": "lmbp-conformance", "models": []string{"m"}, "max_concurrency": 1,
			"proof": lmbp.SignProof(key, r.devID, lmbp.ProofHello, at, time.Now())}
		if mut != nil {
			mut(h)
		}
		return h
	}
	// W9: hello with v 1.
	// An LMBP 1.x device offers "lmbp" (accepted) and is told on hello.
	f, closed, err := r.rawHelloThen(at, 1, hello(nil))
	r.check("W9", "an LMBP 1.x hello (subprotocol lmbp, v 1) gets error VERSION_UNSUPPORTED (same id), no welcome, then the gateway closes",
		want(err == nil && f.Type == lmbp.TypeError && f.ID == "hv" && errBody(f).Code == lmbp.ErrVersion && closed, "frame %+v closed %v err %v", f, closed, err))
	// W10: max_concurrency outside 1..16 (or missing / not an integer) is rejected, never clamped.
	var bad []string
	for name, mut := range map[string]func(map[string]any){
		"0":       func(h map[string]any) { h["max_concurrency"] = 0 },
		"17":      func(h map[string]any) { h["max_concurrency"] = 17 },
		"missing": func(h map[string]any) { delete(h, "max_concurrency") },
		`"2"`:     func(h map[string]any) { h["max_concurrency"] = "2" },
	} {
		f, err := r.rawHello(at, lmbp.Version, hello(mut))
		if err != nil || f.Type != lmbp.TypeError || errBody(f).Code != lmbp.ErrBadRequest {
			bad = append(bad, fmt.Sprintf("max_concurrency %s: %s %s", name, f.Type, f.Data))
		}
	}
	r.check("W10", "hello max_concurrency 0, 17, missing or \"2\" is rejected with error BAD_REQUEST (no welcome, no clamping)", want(len(bad) == 0, "%s", strings.Join(bad, "; ")))
	// W11: display names count code points: 129 BMP characters are too many.
	f, err = r.rawHello(at, lmbp.Version, hello(func(h map[string]any) { h["model_names"] = map[string]string{"m": strings.Repeat("名", 129)} }))
	r.check("W11", "hello whose model_names value has 129 code points is rejected with error BAD_REQUEST",
		want(err == nil && f.Type == lmbp.TypeError && errBody(f).Code == lmbp.ErrBadRequest, "frame %+v err %v", f, err))
}

// awaitTypes reads frames from ch until one has a type in types (or timeout).
func awaitTypes(ch <-chan lmbp.Envelope, d time.Duration, pred func(lmbp.Envelope) bool) (lmbp.Envelope, bool) {
	tm := time.After(d)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return f, false
			}
			if pred(f) {
				return f, true
			}
		case <-tm:
			return lmbp.Envelope{}, false
		}
	}
}

// runV2Session runs on the main session (no flow/resume negotiated) after W7;
// frames are read from the session directly.
func (r *runner) runV2Session(conn *websocket.Conn, wel lmbp.Welcome, declared int) {
	r.check("W12", "welcome.max_concurrency is between 1 and the declared value", want(wel.MaxConcurrency >= 1 && wel.MaxConcurrency <= declared, "declared %d welcome %d", declared, wel.MaxConcurrency))
	// F2: a frame whose v is not 2 after the handshake: error BAD_REQUEST, session kept.
	_ = conn.SetWriteDeadline(time.Now().Add(r.cfg.Timeout))
	_ = conn.WriteJSON(map[string]any{"v": 1, "type": "ping", "id": "old"})
	f1, err1 := r.readFrame(conn)
	_ = r.writeFrame(conn, "ping", "p2", "", nil)
	f2, err2 := r.readFrame(conn)
	r.check("F2", "after the handshake a frame with v 1 is answered by error BAD_REQUEST and the session stays usable (ping → pong)",
		want(err1 == nil && f1.Type == lmbp.TypeError && errBody(f1).Code == lmbp.ErrBadRequest && err2 == nil && f2.Type == lmbp.TypePong && f2.ID == "p2", "got %+v / %+v (%v %v)", f1, f2, err1, err2))
	// W13: models.update with max_concurrency 99 is rejected whole.
	_ = r.writeFrame(conn, "models.update", "mu", "", map[string]any{"models": []string{"m", "extra"}, "max_concurrency": 99})
	f, err := r.readFrame(conn)
	_, dl, _, _ := r.tenant(http.MethodGet, "/lmbp/v1/tenant/devices", nil)
	var dev map[string]any
	if ds, _ := dl["devices"].([]any); ds != nil {
		for _, x := range ds {
			if m, _ := x.(map[string]any); m["device_id"] == r.devID {
				dev = m
			}
		}
	}
	mc, _ := dev["max_concurrency"].(float64)
	ms, _ := dev["models"].([]any)
	r.check("W13", "models.update with max_concurrency 99 is answered by error BAD_REQUEST and not applied (concurrency and models unchanged)",
		want(err == nil && f.Type == lmbp.TypeError && errBody(f).Code == lmbp.ErrBadRequest && int(mc) == wel.MaxConcurrency && len(ms) == 1, "frame %+v err %v device %v", f, err, dev))
}

// runV2Pairing checks device_name length in code points (L1).
func (r *runner) runV2Pairing() {
	try := func(name string) int {
		pub, _, _ := ed25519.GenerateKey(nil)
		st, _, _, _ := r.form("/lmbp/v1/device/authorize", url.Values{"tenant": {r.cfg.Tenant}, "public_key": {lmbp.B64(pub)}, "device_name": {name}})
		return st
	}
	emoji, cjk := try(strings.Repeat("😀", 100)), try(strings.Repeat("名", 101))
	r.check("L1", "device_name counts code points: 100 emoji (400 UTF-8 bytes, 200 UTF-16 units) are accepted, 101 CJK characters are rejected (invalid_request)",
		want(emoji == 200 && cjk == 400, "100 emoji → %d, 101 CJK → %d", emoji, cjk))
}

// runV2Headers checks the X-End-User rule (L2): 1-128 printable ASCII.
func (r *runner) runV2Headers() {
	get := func(user string) int {
		req, _ := http.NewRequest(http.MethodGet, r.cfg.GatewayURL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+r.cfg.TenantKey)
		req.Header["X-End-User"] = []string{user}
		st, _, _, err := r.do(req)
		if err != nil {
			return 0
		}
		return st
	}
	ok, long, nonASCII := get(strings.Repeat("u", 128)), get(strings.Repeat("u", 129)), get("张三")
	r.check("L2", "X-End-User is 1-128 printable ASCII: 128 characters → 200, 129 → 400, non-ASCII (UTF-8 张三) → 400",
		want(ok == 200 && long == 400 && nonASCII == 400, "128 → %d, 129 → %d, 张三 → %d", ok, long, nonASCII))
}

// runV2Ext runs on the extension session (flow and resume negotiated).
func (r *runner) runV2Ext(ctx context.Context, dev *extDev, full func(string) string, mPlain string) {
	chat := func(content string) map[string]any {
		return map[string]any{"model": full(mPlain), "messages": []any{map[string]any{"role": "user", "content": content}}}
	}
	// L3: Idempotency-Key values outside 1-255 printable ASCII are rejected before dispatch.
	keyStatus := func(k string) int {
		cctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
		defer cancel()
		ch := make(chan int, 1)
		go func() { st, _, _ := r.chatRaw(cctx, chat("key"), map[string]string{"Idempotency-Key": k}); ch <- st }()
		for {
			select {
			case st := <-ch:
				return st
			case f := <-dev.frames: // dispatched: answer so the request ends
				if f.Type == lmbp.TypeTaskRun {
					_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 1, lmbp.TaskDone{FinishReason: "stop"})
				}
			}
		}
	}
	long, nonASCII := keyStatus(strings.Repeat("k", 256)), keyStatus("ключ-1")
	r.check("L3", "Idempotency-Key is 1-255 printable ASCII: 256 characters → 400, non-ASCII → 400 (not dispatched)",
		want(long == 400 && nonASCII == 400, "256 → %d, non-ASCII → %d", long, nonASCII))

	noAck := func(taskID string) (cancelReason string, acked bool) {
		deadline := time.Now().Add(1500 * time.Millisecond)
		for time.Now().Before(deadline) {
			f, ok := awaitTypes(dev.frames, time.Until(deadline), func(e lmbp.Envelope) bool { return e.TaskID == taskID })
			if !ok {
				break
			}
			switch f.Type {
			case lmbp.TypeTaskAck:
				acked = true
			case lmbp.TypeTaskCancel:
				var c lmbp.TaskCancel
				_ = json.Unmarshal(f.Data, &c)
				cancelReason = c.Reason
			}
		}
		return
	}
	// S1: a data frame for a task the gateway never knew.
	ghost := "task_ghost_" + lmbp.RandomID("")
	_ = dev.send(lmbp.TypeTaskDelta, ghost, 1, lmbp.TaskDelta{Text: "?"})
	reason, acked := noAck(ghost)
	r.check("S1", "a data frame for an unknown task is answered by task.cancel {reason: unknown_task} and is not ACKed (§8.6 row U1)",
		want(reason == "unknown_task" && !acked, "cancel reason %q acked %v", reason, acked))
	// S2: a frame after the task's terminal frame (finished normally).
	done := make(chan int, 1)
	go func() { st, _, _ := r.chatRaw(ctx, chat("finish"), nil); done <- st }()
	if f, _, err := dev.await(lmbp.TypeTaskRun, nil); err == nil {
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 1, lmbp.TaskDelta{Text: "ok"})
		_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 2, lmbp.TaskDone{FinishReason: "stop"})
		st := <-done
		var hi int64
		for {
			a, ok := awaitTypes(dev.frames, time.Second, func(e lmbp.Envelope) bool { return e.TaskID == f.TaskID && e.Type == lmbp.TypeTaskAck })
			if !ok {
				break
			}
			var ta lmbp.TaskAck
			_ = json.Unmarshal(a.Data, &ta)
			if hi = max(hi, ta.Seq); hi >= 2 {
				break
			}
		}
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 3, lmbp.TaskDelta{Text: "after end"})
		reason, acked := noAck(f.TaskID)
		r.check("S2", "frames 1-2 of a finished task are ACKed; a frame after its task.done is answered by task.cancel unknown_task, not ACKed (§8.6 rows R2, F1)",
			want(st == 200 && hi >= 2 && reason == "unknown_task" && !acked, "status %d highest ack %d; after end: cancel %q acked %v", st, hi, reason, acked))
	} else {
		r.check("S2", "finished task", err)
	}
	// S3: a locally ended task drains until its terminal frame, then is forgotten.
	cctx, cancel := context.WithCancel(ctx)
	go func() { _, _, _ = r.chatRaw(cctx, chat("drain"), nil) }()
	if f, _, err := dev.await(lmbp.TypeTaskRun, nil); err == nil {
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 1, lmbp.TaskDelta{Text: "x"})
		time.Sleep(300 * time.Millisecond)
		cancel()
		_, _, cerr := dev.await(lmbp.TypeTaskCancel, func(e lmbp.Envelope) bool { return e.TaskID == f.TaskID })
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 2, lmbp.TaskDelta{Text: "late"})
		_ = dev.send(lmbp.TypeTaskError, f.TaskID, 3, lmbp.ErrorBody{Code: "CANCELLED", Message: "cancelled"})
		var hi int64
		for {
			a, ok := awaitTypes(dev.frames, 2*time.Second, func(e lmbp.Envelope) bool { return e.TaskID == f.TaskID && e.Type == lmbp.TypeTaskAck })
			if !ok {
				break
			}
			var ta lmbp.TaskAck
			_ = json.Unmarshal(a.Data, &ta)
			if hi = max(hi, ta.Seq); hi >= 3 {
				break
			}
		}
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 4, lmbp.TaskDelta{Text: "after terminal"})
		reason, acked := noAck(f.TaskID)
		r.check("S3", "after task.cancel the late frames up to the terminal task.error are ACKed; a frame after that terminal frame gets task.cancel unknown_task (§8.6 rows D1, D2, F1)",
			want(cerr == nil && hi >= 3 && reason == "unknown_task" && !acked, "cancel %v highest ack %d; after terminal: cancel %q acked %v", cerr, hi, reason, acked))
	} else {
		cancel()
		r.check("S3", "draining task", err)
	}
}
