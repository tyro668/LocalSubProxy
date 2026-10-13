// Package fakemodel is a tiny OpenAI-compatible model server for demos and
// tests. It streams the reply character by character. Model "broken" fails.
package fakemodel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Server struct {
	Models []string
	Delay  time.Duration // per chunk
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		data := []map[string]string{}
		for _, id := range s.Models {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})
	m.HandleFunc("/v1/chat/completions", s.chat)
	return m
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role, Content string
		} `json:"messages"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Messages) == 0 {
		http.Error(w, `{"error":{"message":"bad request"}}`, 400)
		return
	}
	if req.Model == "broken" {
		http.Error(w, `{"error":{"message":"model crashed"}}`, 500)
		return
	}
	last := req.Messages[len(req.Messages)-1].Content
	reply := fmt.Sprintf("你好，我是本机模型 %s。你说的是：%s", req.Model, last)
	if strings.Contains(last, "长") {
		reply = strings.Repeat("这是一段很长的回答。", 50)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	n := 0
	for _, ch := range reply {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(s.Delay):
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": string(ch)}}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if f != nil {
			f.Flush()
		}
		n++
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{}, "finish_reason": "stop"}},
		"usage": map[string]int{"prompt_tokens": len([]rune(last)), "completion_tokens": n}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
}
