package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Checks for the protocol 1.0 errata E1–E7 (spec appendix A: §6.6 routing,
// §6 Cache-Control, §6.5 non-streaming disconnect, §16.2 remote images,
// §16.5 ignored vs rejected fields, §14.4 retention, §15.5 late frames).
// The check ids match the errata numbers. Every implementation must agree on
// these; the 2026-10 SDK consistency review found several of them divergent.
func (r *runner) runErrata(ctx context.Context, dev *extDev, full func(string) string, mTools, mPlain string) {
	// E1: §6.6 a named, online device that does not declare the model.
	st, h, b := r.chatRaw(ctx, map[string]any{"model": full("not-declared"), "messages": []any{map[string]any{"role": "user", "content": "x"}}}, nil)
	r.check("E1", "a model the named online device does not declare is 404 MODEL_NOT_FOUND (not 503 DEVICE_OFFLINE)",
		want(st == 404 && errCode(b) == lmbp.ErrModelNotFound, "status %d %s", st, b))
	ast, _, ab := r.messagesRaw(ctx, map[string]any{"model": full("not-declared"), "max_tokens": 5, "messages": []any{map[string]any{"role": "user", "content": "x"}}}, nil)
	r.check("E1b", "the Messages API answers the same request with 404 not_found_error",
		want(ast == 404 && anthErrType(ab) == "not_found_error", "status %d %s", ast, ab))

	// E2: §6 every JSON response is Cache-Control: no-store.
	var bad []string
	note := func(what string, h http.Header) {
		if h == nil || !strings.Contains(strings.ToLower(h.Get("Cache-Control")), "no-store") {
			cc := ""
			if h != nil {
				cc = h.Get("Cache-Control")
			}
			bad = append(bad, fmt.Sprintf("%s: Cache-Control=%q", what, cc))
		}
	}
	note("404 chat error", h)
	_, _, mh, _ := r.tenantH(http.MethodGet, "/v1/models")
	note("GET /v1/models", mh)
	_, _, dh, _ := r.tenantH(http.MethodGet, "/lmbp/v1/tenant/devices")
	note("GET /lmbp/v1/tenant/devices", dh)
	req, _ := http.NewRequest(http.MethodGet, r.cfg.GatewayURL+"/v1/models", nil)
	_, _, uh, err := r.do(req)
	if err == nil {
		note("401 without tenant key", uh)
	}
	_, _, hh, err := r.do(mustReq(http.MethodGet, r.cfg.GatewayURL+"/healthz"))
	if err == nil {
		note("GET /healthz", hh)
	}
	ch := make(chan http.Header, 1)
	go func() {
		_, h, _ := r.chatRaw(ctx, map[string]any{"model": full(mPlain), "messages": []any{map[string]any{"role": "user", "content": "no-store"}}}, nil)
		ch <- h
	}()
	if f, _, err := dev.await(lmbp.TypeTaskRun, nil); err == nil {
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 1, lmbp.TaskDelta{Text: "ok"})
		_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 2, lmbp.TaskDone{FinishReason: "stop"})
		note("200 chat completion", <-ch)
	} else {
		bad = append(bad, "200 chat completion: "+err.Error())
	}
	r.check("E2", "JSON responses (results, errors, lists, healthz) carry Cache-Control: no-store", want(len(bad) == 0, "%s", strings.Join(bad, "; ")))

	// E3: §6.5 a non-streaming caller that disconnects cancels the task.
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		_, _, _ = r.chatRaw(cctx, map[string]any{"model": full(mPlain), "stream": false, "messages": []any{map[string]any{"role": "user", "content": "leave"}}}, nil)
	}()
	if f, _, err := dev.await(lmbp.TypeTaskRun, nil); err == nil {
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 1, lmbp.TaskDelta{Text: "x"})
		time.Sleep(300 * time.Millisecond)
		cancel()
		t0 := time.Now()
		c, _, err := dev.await(lmbp.TypeTaskCancel, func(e lmbp.Envelope) bool { return e.TaskID == f.TaskID })
		var tc lmbp.TaskCancel
		if err == nil {
			_ = json.Unmarshal(c.Data, &tc)
		}
		el := time.Since(t0)
		r.check("E3", "a non-streaming caller that disconnects gets its task cancelled within 3s (task.cancel reason client_disconnected)",
			want(err == nil && tc.Reason == "client_disconnected" && el < 3*time.Second, "err %v reason %q after %v", err, tc.Reason, el.Round(time.Millisecond)))
		// E7: §15.5 frames the device sends after the gateway ended the task
		// locally are still ACKed (they release the device's resume buffer
		// and flow credit); they are never answered with task.cancel(unknown_task).
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 2, lmbp.TaskDelta{Text: "late"})
		_ = dev.send(lmbp.TypeTaskError, f.TaskID, 3, lmbp.ErrorBody{Code: "CANCELLED", Message: "cancelled"})
		var acked int64
		deadline := time.Now().Add(r.cfg.Timeout)
		for acked < 3 && time.Now().Before(deadline) {
			e, _, err := dev.await(lmbp.TypeTaskAck, func(e lmbp.Envelope) bool { return e.TaskID == f.TaskID })
			if err != nil {
				break
			}
			var a lmbp.TaskAck
			_ = json.Unmarshal(e.Data, &a)
			acked = max(acked, a.Seq)
		}
		r.check("E7", "late frames of a task the gateway ended locally are ACKed (credit released), not answered with unknown_task",
			want(acked >= 3, "highest task.ack seq %d for late frames 2..3", acked))
	} else {
		cancel()
		r.check("E3", "non-streaming disconnect", err)
	}

	// E4: §16.2 a device that cannot fetch a remote image answers
	// CAPABILITY_UNSUPPORTED; the caller sees 400 with that code.
	img := map[string]any{"model": full(mTools), "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/cat.png"}}}}}}
	ich := make(chan [2]any, 1)
	go func() { s, _, b := r.chatRaw(ctx, img, nil); ich <- [2]any{s, b} }()
	if f, _, err := dev.await(lmbp.TypeTaskRun, nil); err == nil {
		run, verr := lmbp.VerifyTaskRun(r.gwKey, f.TaskID, f.JWS)
		mj, _ := json.Marshal(run.Messages)
		urlKept := verr == nil && strings.Contains(string(mj), "https://example.com/cat.png")
		_ = dev.send(lmbp.TypeTaskError, f.TaskID, 1, lmbp.ErrorBody{Code: lmbp.ErrUnsupported, Message: "remote image URLs are not supported"})
		res := <-ich
		s, b := res[0].(int), res[1].([]byte)
		r.check("E4", "a remote image URL is forwarded unchanged and a device CAPABILITY_UNSUPPORTED answer becomes 400 CAPABILITY_UNSUPPORTED",
			want(urlKept && s == 400 && errCode(b) == lmbp.ErrUnsupported, "url forwarded %v (%v); status %d %s", urlKept, verr, s, b))
	} else {
		r.check("E4", "remote image URL", err)
	}

	// E5: §16.5 ignored request fields are accepted and never reach the device.
	ign := func(path string, body map[string]any, fields ...string) error {
		rch := make(chan int, 1)
		go func() {
			var s int
			if path == "/v1/messages" {
				s, _, _ = r.messagesRaw(ctx, body, nil)
			} else {
				s, _, _ = r.chatRaw(ctx, body, nil)
			}
			rch <- s
		}()
		f, _, err := dev.await(lmbp.TypeTaskRun, nil)
		if err != nil {
			return fmt.Errorf("%s: %v (status %d)", path, err, <-rch)
		}
		var leaked []string
		if parts := strings.Split(f.JWS, "."); len(parts) == 3 {
			if p, err := lmbp.UnB64(parts[1]); err == nil {
				var m map[string]any
				_ = json.Unmarshal(p, &m)
				for _, k := range fields {
					if _, ok := m[k]; ok {
						leaked = append(leaked, k)
					}
				}
			}
		}
		_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 1, lmbp.TaskDelta{Text: "ok"})
		_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 2, lmbp.TaskDone{FinishReason: "stop"})
		if s := <-rch; s != 200 || len(leaked) > 0 {
			return fmt.Errorf("%s: status %d, forwarded %v", path, s, leaked)
		}
		return nil
	}
	e1 := ign("/v1/chat/completions", map[string]any{"model": full(mPlain), "messages": []any{map[string]any{"role": "user", "content": "x"}},
		"response_format": map[string]any{"type": "json_object"}, "seed": 7, "logprobs": true, "presence_penalty": 0.5, "user": "u1"}, "response_format", "seed", "logprobs", "presence_penalty", "user")
	e2 := ign("/v1/messages", map[string]any{"model": full(mPlain), "max_tokens": 5, "messages": []any{map[string]any{"role": "user", "content": "x"}},
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}, "metadata": map[string]any{"user_id": "u1"}, "service_tier": "auto"}, "thinking", "metadata", "service_tier")
	r.check("E5", "ignored fields (response_format, seed, logprobs, presence_penalty, user; thinking, metadata, service_tier) are accepted and not forwarded",
		want(e1 == nil && e2 == nil, "%v; %v", e1, e2))

	// E6: §14.4 retention. Only when the gateway under test runs with a short
	// retention (-retention): a finished keyed task replays from its record,
	// and after the retention period the record is gone and the same key and
	// body run again.
	if r.cfg.Retention <= 0 {
		r.skip("E6", "retention purges finished tasks with their idempotency records", "run with -retention <gateway retention> to enable")
		return
	}
	key := fmt.Sprintf("conf-retention-%d", time.Now().UnixNano())
	body := map[string]any{"model": full(mPlain), "messages": []any{map[string]any{"role": "user", "content": "retention"}}}
	once := func(answer string) (bool, int, string) {
		rch := make(chan [2]any, 1)
		go func() {
			s, _, b := r.chatRaw(ctx, body, map[string]string{"Idempotency-Key": key})
			rch <- [2]any{s, b}
		}()
		// A replay answers without a task.run: stop waiting as soon as the
		// HTTP response is in. Other frames (acks of earlier checks) are skipped.
		ran := false
		runs := make(chan lmbp.Envelope, 1)
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				case f, ok := <-dev.frames:
					if !ok {
						return
					}
					if f.Type == lmbp.TypeTaskRun {
						runs <- f
						return
					}
				}
			}
		}()
		var res [2]any
		select {
		case res = <-rch:
		case f := <-runs:
			ran = true
			_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 1, lmbp.TaskDelta{Text: answer})
			_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 2, lmbp.TaskDone{FinishReason: "stop"})
			res = <-rch
		}
		close(stop)
		return ran, res[0].(int), string(res[1].([]byte))
	}
	ran1, s1, b1 := once("first")
	ran2, s2, b2 := once("second")
	time.Sleep(3*r.cfg.Retention + time.Second)
	ran3, s3, b3 := once("third")
	r.check("E6", "retention purges finished tasks with their idempotency records: an immediate replay re-attaches, a replay after the retention period runs again",
		want(ran1 && s1 == 200 && strings.Contains(b1, "first") && !ran2 && s2 == 200 && strings.Contains(b2, "first") && ran3 && s3 == 200 && strings.Contains(b3, "third"),
			"first ran=%v %d; replay ran=%v %d %.80s; after retention ran=%v %d %.80s", ran1, s1, ran2, s2, b2, ran3, s3, b3))
}

func mustReq(method, url string) *http.Request {
	req, _ := http.NewRequest(method, url, nil)
	return req
}

func (r *runner) tenantH(method, path string) (int, map[string]any, http.Header, error) {
	return r.tenant(method, path, nil)
}
