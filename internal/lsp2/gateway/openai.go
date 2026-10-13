package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"localsubproxy/internal/lsp2"
	"localsubproxy/internal/lsp2/store"
)

// models lists "device_id/model" plus bare model names for the caller's own
// online devices only (tenant + owner scoped).
func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	t, user, ok := g.tenantAuth(w, r)
	if !ok {
		return
	}
	ds, err := g.cfg.Store.ListDevices(r.Context(), t.ID, user)
	if err != nil {
		apiError(w, 500, lsp2.ErrInternal, "storage error")
		return
	}
	data := []map[string]any{}
	for _, d := range ds {
		if s := g.online(d.ID); s != nil && d.Status == "active" {
			for _, m := range s.modelList() {
				data = append(data, map[string]any{"id": d.ID + "/" + m, "object": "model", "owned_by": d.Name})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

type chatRequest struct {
	Model       string         `json:"model"`
	Messages    []lsp2.Message `json:"messages"`
	Stream      bool           `json:"stream"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	// Unknown OpenAI fields are ignored rather than rejected.
}

// resolve picks the caller's device for "device_id/model" or a bare model name.
func (g *Gateway) resolve(ctx context.Context, tenant, user, model string) (*session, string, string, int) {
	ds, err := g.cfg.Store.ListDevices(ctx, tenant, user)
	if err != nil {
		return nil, "", lsp2.ErrInternal, 500
	}
	want, bare := model, true
	devID := ""
	if i := strings.Index(model, "/"); i > 0 && strings.HasPrefix(model, "dev_") {
		devID, want, bare = model[:i], model[i+1:], false
	}
	known := false
	for _, d := range ds {
		if d.Status != "active" || (!bare && d.ID != devID) {
			continue
		}
		known = known || !bare
		if s := g.online(d.ID); s != nil && contains(s.modelList(), want) {
			return s, want, "", 0
		}
	}
	if known {
		return nil, "", lsp2.ErrDeviceOffline, 503
	}
	if len(ds) > 0 && bare {
		online := false
		for _, d := range ds {
			online = online || g.online(d.ID) != nil
		}
		if !online {
			return nil, "", lsp2.ErrDeviceOffline, 503
		}
	}
	return nil, "", lsp2.ErrModelNotFound, 404
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, 405, lsp2.ErrBadRequest, "POST required")
		return
	}
	t, user, ok := g.tenantAuth(w, r)
	if !ok {
		return
	}
	var req chatRequest
	if err := decode(r, &req, 1<<20); err != nil || req.Model == "" || len(req.Messages) == 0 || len(req.Messages) > 200 {
		apiError(w, 400, lsp2.ErrBadRequest, "model and 1-200 messages required")
		return
	}
	for _, m := range req.Messages {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			apiError(w, 400, lsp2.ErrBadRequest, "unsupported role "+m.Role)
			return
		}
	}
	s, model, code, status := g.resolve(r.Context(), t.ID, user, req.Model)
	if s == nil {
		apiError(w, status, code, map[string]string{lsp2.ErrDeviceOffline: "your device is offline", lsp2.ErrModelNotFound: "model not found on your devices"}[code])
		return
	}
	taskID := lsp2.RandomID("task_")
	ch := make(chan lsp2.Envelope, 1024)
	s.mu.Lock()
	if len(s.runs) >= s.maxConc {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "2")
		apiError(w, 429, lsp2.ErrDeviceBusy, "device is at max concurrency")
		return
	}
	s.runs[taskID] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.runs, taskID); s.mu.Unlock() }()

	run := lsp2.TaskRun{TaskID: taskID, Iat: time.Now().Unix(), DeviceID: s.deviceID, Model: model, Messages: req.Messages, MaxTokens: req.MaxTokens, Temperature: req.Temperature,
		Exp: time.Now().Add(time.Minute).Unix(), Nonce: lsp2.RandomID("n_")}
	data, _ := json.Marshal(run)
	ctx := context.WithoutCancel(r.Context())
	now := time.Now()
	if err := g.cfg.Store.CreateTask(ctx, store.Task{ID: taskID, TenantID: t.ID, OwnerID: user, DeviceID: s.deviceID, Model: model, State: "RUNNING", Request: data, CreatedAt: now, UpdatedAt: now}); err != nil {
		apiError(w, 500, lsp2.ErrInternal, "storage error")
		return
	}
	jws := lsp2.SignTaskRun(g.cfg.SigningKey, g.kid, run)
	if err := s.send(lsp2.Envelope{V: lsp2.Version, Type: lsp2.TypeTaskRun, TaskID: taskID, JWS: jws}); err != nil {
		_ = g.cfg.Store.SetTaskState(ctx, taskID, "FAILED")
		apiError(w, 503, lsp2.ErrDeviceOffline, "device disconnected")
		return
	}
	w.Header().Set("X-Task-Id", taskID)

	created := now.Unix()
	chunk := func(delta map[string]any, finish any, usage *lsp2.Usage) []byte {
		m := map[string]any{"id": "chatcmpl-" + taskID, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			m["usage"] = usage
		}
		b, _ := json.Marshal(m)
		return b
	}
	var flusher http.Flusher
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, _ = w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil))
		if flusher != nil {
			flusher.Flush()
		}
	}
	var text strings.Builder
	var lastSeq int64
	timeout := time.NewTimer(g.cfg.TaskTimeout)
	defer timeout.Stop()
	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	fail := func(state string, eb lsp2.ErrorBody, status int) {
		_ = g.cfg.Store.SetTaskState(ctx, taskID, state)
		if req.Stream {
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": eb.Message, "type": "lsp2_error", "code": eb.Code}})
			fmt.Fprintf(w, "data: %s\n\n", b) // no [DONE] after an error
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		apiError(w, status, eb.Code, eb.Message)
	}
	cancel := func(reason string) {
		b, _ := json.Marshal(map[string]string{"reason": reason})
		_ = s.send(lsp2.Envelope{V: lsp2.Version, Type: lsp2.TypeTaskCancel, TaskID: taskID, Data: b})
	}
	for {
		select {
		case <-r.Context().Done():
			cancel("client_disconnected")
			_ = g.cfg.Store.SetTaskState(ctx, taskID, "CANCELLED")
			return
		case <-timeout.C:
			cancel("timeout")
			fail("FAILED", lsp2.ErrorBody{Code: lsp2.ErrTimeout, Message: "task timed out"}, 504)
			return
		case <-s.closed:
			fail("FAILED", lsp2.ErrorBody{Code: lsp2.ErrDeviceOffline, Message: "device disconnected mid-task", Retryable: true}, 503)
			return
		case <-keep.C:
			if req.Stream {
				fmt.Fprint(w, ": keep-alive\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
		case e := <-ch:
			if e.Seq != lastSeq+1 {
				cancel("protocol_error")
				fail("FAILED", lsp2.ErrorBody{Code: lsp2.ErrInternal, Message: "sequence gap from device"}, 502)
				return
			}
			lastSeq = e.Seq
			_ = g.cfg.Store.AppendEvent(ctx, store.Event{TaskID: taskID, Seq: e.Seq, Kind: e.Type, Data: e.Data, At: time.Now()})
			switch e.Type {
			case lsp2.TypeTaskDelta:
				var d lsp2.TaskDelta
				_ = json.Unmarshal(e.Data, &d)
				if req.Stream {
					fmt.Fprintf(w, "data: %s\n\n", chunk(map[string]any{"content": d.Text}, nil, nil))
					if flusher != nil {
						flusher.Flush()
					}
				} else {
					text.WriteString(d.Text)
				}
			case lsp2.TypeTaskDone:
				var d lsp2.TaskDone
				_ = json.Unmarshal(e.Data, &d)
				if d.FinishReason == "" {
					d.FinishReason = "stop"
				}
				_ = g.cfg.Store.SetTaskState(ctx, taskID, "SUCCEEDED")
				if req.Stream {
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk(map[string]any{}, d.FinishReason, d.Usage))
					if flusher != nil {
						flusher.Flush()
					}
					return
				}
				resp := map[string]any{"id": "chatcmpl-" + taskID, "object": "chat.completion", "created": created, "model": req.Model,
					"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": text.String()}, "finish_reason": d.FinishReason}}}
				if d.Usage != nil {
					resp["usage"] = d.Usage
				}
				writeJSON(w, 200, resp)
				return
			case lsp2.TypeTaskError:
				var eb lsp2.ErrorBody
				_ = json.Unmarshal(e.Data, &eb)
				state := "FAILED"
				if eb.Code == lsp2.ErrCancelled {
					state = "CANCELLED"
				}
				fail(state, eb, 502)
				return
			}
		}
	}
}
