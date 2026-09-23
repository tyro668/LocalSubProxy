package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"localsubproxy/internal/protocol"
)

// The Remote-service-facing Chat Completions subset uses adapterID/modelID as its model
// name. A provider model ID may itself contain slashes.
func (s *Server) openAIModels(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, false) || !method(w, r, http.MethodGet) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data := make([]map[string]any, 0)
	if time.Since(s.lastSeen) < 75*time.Second {
		for _, a := range s.adapters {
			if a.Health != "ready" || !contains(a.Operations, "chat.generate") {
				continue
			}
			for _, model := range a.Models {
				data = append(data, map[string]any{"id": a.AdapterID + "/" + model, "object": "model", "owned_by": a.AdapterID})
			}
		}
	}
	reply(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Server) openAIChat(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, false) || !method(w, r, http.MethodPost) {
		return
	}
	var input struct {
		Model    string             `json:"model"`
		Messages []protocol.Message `json:"messages"`
		Stream   bool               `json:"stream"`
	}
	if !decode(w, r, &input, 128<<10) {
		return
	}
	adapterID, modelID, ok := strings.Cut(input.Model, "/")
	if !ok || adapterID == "" || modelID == "" {
		http.Error(w, "model must be adapterID/modelID", 422)
		return
	}
	req := protocol.CreateTask{Target: protocol.Target{DeviceID: s.cfg.DeviceID, AdapterID: adapterID, ModelID: modelID}, Operation: "chat.generate", Input: protocol.Input{Messages: input.Messages}}
	req.Options.QueueTTLSeconds = 300
	req.Options.MaxRuntimeSeconds = 600
	if err := req.Validate(); err != nil {
		http.Error(w, err.Error(), 422)
		return
	}
	s.mu.Lock()
	if s.state.Revoked || s.cfg.RequireBinding && s.state.OwnerID == "" {
		s.mu.Unlock()
		http.Error(w, "device unavailable", 403)
		return
	}
	a, exists := s.adapters[adapterID]
	if !exists || time.Since(s.lastSeen) >= 75*time.Second || a.Health != "ready" || !contains(a.Operations, "chat.generate") || !contains(a.Models, modelID) {
		s.mu.Unlock()
		http.Error(w, "model unavailable", 422)
		return
	}
	now := time.Now().UTC()
	task := protocol.Task{ID: randomID("task_"), State: "QUEUED", Request: req, Digest: protocol.Digest(req), CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(300 * time.Second)}
	s.state.Tasks[task.ID] = &record{Task: task}
	if err := s.persist(); err != nil {
		delete(s.state.Tasks, task.ID)
		s.mu.Unlock()
		http.Error(w, "storage error", 503)
		return
	}
	s.notify()
	s.mu.Unlock()
	if input.Stream {
		s.openAIStream(w, r, task.ID, input.Model)
		return
	}
	s.openAIResponse(w, r, task.ID, input.Model)
}

func (s *Server) taskSnapshot(id string) (protocol.Task, <-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.expireLocked(); err != nil {
		return protocol.Task{}, nil, err
	}
	t := s.state.Tasks[id]
	if t == nil {
		return protocol.Task{}, nil, errors.New("task missing")
	}
	copy := t.Task
	copy.Events = append([]protocol.Event(nil), t.Task.Events...)
	return copy, s.changed, nil
}

func (s *Server) openAIResponse(w http.ResponseWriter, r *http.Request, id, model string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		t, changed, err := s.taskSnapshot(id)
		if err != nil {
			http.Error(w, "storage error", 503)
			return
		}
		if terminal(t.State) {
			if t.State != "SUCCEEDED" {
				http.Error(w, "model task "+strings.ToLower(t.State), 502)
				return
			}
			var content strings.Builder
			for _, event := range t.Events {
				if event.Kind == "output.delta" {
					var data struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(event.Data, &data) == nil {
						content.WriteString(data.Text)
					}
				}
			}
			reply(w, 200, map[string]any{"id": "chatcmpl_" + id, "object": "chat.completion", "created": t.CreatedAt.Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": content.String()}, "finish_reason": "stop"}}})
			return
		}
		select {
		case <-changed:
		case <-ticker.C:
		case <-r.Context().Done():
			s.cancel(httptest.NewRecorder(), id)
			return
		}
	}
}

func (s *Server) openAIStream(w http.ResponseWriter, r *http.Request, id, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	seq := 0
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		t, changed, err := s.taskSnapshot(id)
		if err != nil {
			_, _ = io.WriteString(w, "event: error\ndata: storage error\n\n")
			flusher.Flush()
			return
		}
		for seq < len(t.Events) {
			event := t.Events[seq]
			seq++
			if event.Kind != "output.delta" {
				continue
			}
			var delta struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Data, &delta) != nil {
				continue
			}
			chunk := map[string]any{"id": "chatcmpl_" + id, "object": "chat.completion.chunk", "created": t.CreatedAt.Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": delta.Text}, "finish_reason": nil}}}
			b, _ := json.Marshal(chunk)
			_, _ = w.Write(append(append([]byte("data: "), b...), []byte("\n\n")...))
			flusher.Flush()
		}
		if terminal(t.State) {
			if t.State != "SUCCEEDED" {
				_, _ = io.WriteString(w, "event: error\ndata: model task "+strings.ToLower(t.State)+"\n\n")
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		select {
		case <-changed:
		case <-ticker.C:
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			s.cancel(httptest.NewRecorder(), id)
			return
		}
	}
}
