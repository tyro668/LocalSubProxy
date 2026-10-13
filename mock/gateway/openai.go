package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"localsubproxy/mock/store"
	"localsubproxy/mock/v2proto"
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
		apiError(w, 500, v2proto.ErrInternal, "storage error")
		return
	}
	data := []map[string]any{}
	for _, d := range ds {
		if s := g.online(d.ID); s != nil && d.Status == "active" {
			for _, m := range s.models {
				data = append(data, map[string]any{"id": d.ID + "/" + m, "object": "model", "owned_by": d.Name})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

type chatRequest struct {
	Model       string            `json:"model"`
	Messages    []v2proto.Message `json:"messages"`
	Stream      bool              `json:"stream"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	// Unknown OpenAI fields are ignored rather than rejected.
}

// resolve picks the caller's device for "device_id/model" or a bare model name.
func (g *Gateway) resolve(ctx context.Context, tenant, user, model string) (*session, string, string, int) {
	ds, err := g.cfg.Store.ListDevices(ctx, tenant, user)
	if err != nil {
		return nil, "", v2proto.ErrInternal, 500
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
		if s := g.online(d.ID); s != nil && contains(s.models, want) {
			return s, want, "", 0
		}
	}
	if known {
		return nil, "", v2proto.ErrDeviceOffline, 503
	}
	if len(ds) > 0 && bare {
		online := false
		for _, d := range ds {
			online = online || g.online(d.ID) != nil
		}
		if !online {
			return nil, "", v2proto.ErrDeviceOffline, 503
		}
	}
	return nil, "", v2proto.ErrModelNotFound, 404
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, 405, v2proto.ErrBadRequest, "POST required")
		return
	}
	t, user, ok := g.tenantAuth(w, r)
	if !ok {
		return
	}
	var req chatRequest
	if err := decode(r, &req, 1<<20); err != nil || req.Model == "" || len(req.Messages) == 0 || len(req.Messages) > 200 {
		apiError(w, 400, v2proto.ErrBadRequest, "model and 1-200 messages required")
		return
	}
	for _, m := range req.Messages {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			apiError(w, 400, v2proto.ErrBadRequest, "unsupported role "+m.Role)
			return
		}
	}
	s, model, code, status := g.resolve(r.Context(), t.ID, user, req.Model)
	if s == nil {
		apiError(w, status, code, map[string]string{v2proto.ErrDeviceOffline: "your device is offline", v2proto.ErrModelNotFound: "model not found on your devices"}[code])
		return
	}
	taskID := v2proto.RandomID("task_")
	ch := make(chan v2proto.Envelope, 1024)
	s.mu.Lock()
	if len(s.runs) >= s.maxConc {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "2")
		apiError(w, 429, v2proto.ErrDeviceBusy, "device is at max concurrency")
		return
	}
	s.runs[taskID] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.runs, taskID); s.mu.Unlock() }()

	run := v2proto.TaskRun{DeviceID: s.deviceID, Model: model, Messages: req.Messages, MaxTokens: req.MaxTokens, Temperature: req.Temperature,
		Exp: time.Now().Add(time.Minute).Unix(), Nonce: v2proto.RandomID("n_")}
	data, _ := json.Marshal(run)
	ctx := context.WithoutCancel(r.Context())
	now := time.Now()
	if err := g.cfg.Store.CreateTask(ctx, store.Task{ID: taskID, TenantID: t.ID, OwnerID: user, DeviceID: s.deviceID, Model: model, State: "RUNNING", Request: data, CreatedAt: now, UpdatedAt: now}); err != nil {
		apiError(w, 500, v2proto.ErrInternal, "storage error")
		return
	}
	sig := v2proto.B64(edSign(g, v2proto.TaskRunSigningInput(taskID, data)))
	if err := s.send(v2proto.Envelope{V: v2proto.Version, Type: v2proto.TypeTaskRun, TaskID: taskID, Data: data, Sig: sig}); err != nil {
		_ = g.cfg.Store.SetTaskState(ctx, taskID, "FAILED")
		apiError(w, 503, v2proto.ErrDeviceOffline, "device disconnected")
		return
	}
	w.Header().Set("X-Task-Id", taskID)

	created := now.Unix()
	chunk := func(delta map[string]any, finish any, usage *v2proto.Usage) []byte {
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
	fail := func(state string, eb v2proto.ErrorBody, status int) {
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
		_ = s.send(v2proto.Envelope{V: v2proto.Version, Type: v2proto.TypeTaskCancel, TaskID: taskID, Data: b})
	}
	for {
		select {
		case <-r.Context().Done():
			cancel("client_disconnected")
			_ = g.cfg.Store.SetTaskState(ctx, taskID, "CANCELLED")
			return
		case <-timeout.C:
			cancel("timeout")
			fail("FAILED", v2proto.ErrorBody{Code: v2proto.ErrTimeout, Message: "task timed out"}, 504)
			return
		case <-s.closed:
			fail("FAILED", v2proto.ErrorBody{Code: v2proto.ErrDeviceOffline, Message: "device disconnected mid-task", Retryable: true}, 503)
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
				fail("FAILED", v2proto.ErrorBody{Code: v2proto.ErrInternal, Message: "sequence gap from device"}, 502)
				return
			}
			lastSeq = e.Seq
			_ = g.cfg.Store.AppendEvent(ctx, store.Event{TaskID: taskID, Seq: e.Seq, Kind: e.Type, Data: e.Data, At: time.Now()})
			switch e.Type {
			case v2proto.TypeTaskDelta:
				var d v2proto.TaskDelta
				_ = json.Unmarshal(e.Data, &d)
				if req.Stream {
					fmt.Fprintf(w, "data: %s\n\n", chunk(map[string]any{"content": d.Text}, nil, nil))
					if flusher != nil {
						flusher.Flush()
					}
				} else {
					text.WriteString(d.Text)
				}
			case v2proto.TypeTaskDone:
				var d v2proto.TaskDone
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
			case v2proto.TypeTaskError:
				var eb v2proto.ErrorBody
				_ = json.Unmarshal(e.Data, &eb)
				state := "FAILED"
				if eb.Code == v2proto.ErrCancelled {
					state = "CANCELLED"
				}
				fail(state, eb, 502)
				return
			}
		}
	}
}
