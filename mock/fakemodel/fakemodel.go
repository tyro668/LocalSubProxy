// Package fakemodel is a tiny OpenAI-compatible model server for demos and
// tests. It streams the reply character by character. Behaviour:
//   - model "broken" fails with HTTP 500;
//   - with tools and a user turn: streams a get_weather tool call in two deltas;
//   - after a tool result: answers from the tool output;
//   - image_url parts: describes the image (media type and decoded size);
//   - "长" in the prompt: a long answer; "超长" a very long, fast stream.
package fakemodel

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"localsubproxy/internal/lsp2"
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

// VeryLongChunks is the number of chunks of the "超长" reply.
const VeryLongChunks = 60000

func VeryLongChunk(i int) string { return fmt.Sprintf("[%05d]%s", i, strings.Repeat("流", 90)) }

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string         `json:"model"`
		Messages []lsp2.Message `json:"messages"`
		Tools    []lsp2.Tool    `json:"tools"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Messages) == 0 {
		http.Error(w, `{"error":{"message":"bad request"}}`, 400)
		return
	}
	if req.Model == "broken" {
		http.Error(w, `{"error":{"message":"model crashed"}}`, 500)
		return
	}
	lastMsg := req.Messages[len(req.Messages)-1]
	last := lastMsg.Content.PlainText()
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if f != nil {
			f.Flush()
		}
	}
	finish := func(reason string, n int) {
		send(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{}, "finish_reason": reason}},
			"usage": map[string]int{"prompt_tokens": len([]rune(last)), "completion_tokens": n}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	if len(req.Tools) > 0 && lastMsg.Role == "user" {
		name := req.Tools[0].Function.Name
		send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_fake1", "type": "function", "function": map[string]string{"name": name, "arguments": `{"city":`}}}}}}})
		send(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]string{"arguments": `"上海"}`}}}}}}})
		finish("tool_calls", 2)
		return
	}
	reply := fmt.Sprintf("你好，我是本机模型 %s。你说的是：%s", req.Model, last)
	switch {
	case lastMsg.Role == "tool":
		reply = "根据工具结果：" + last
	case strings.Contains(last, "超长"):
		for i := 0; i < VeryLongChunks; i++ {
			if r.Context().Err() != nil {
				return
			}
			send(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": VeryLongChunk(i)}}}})
		}
		finish("stop", VeryLongChunks)
		return
	case strings.Contains(last, "长"):
		reply = strings.Repeat("这是一段很长的回答。", 50)
	}
	for _, p := range lastMsg.Content.Parts {
		if p.ImageURL != nil {
			desc := "远程图片 " + p.ImageURL.URL
			if rest, ok := strings.CutPrefix(p.ImageURL.URL, "data:"); ok {
				meta, data, _ := strings.Cut(rest, ",")
				raw, _ := base64.StdEncoding.DecodeString(data)
				desc = fmt.Sprintf("%s 图片，%d 字节", strings.TrimSuffix(meta, ";base64"), len(raw))
			}
			reply = "我看到了一张" + desc + "。" + reply
		}
	}
	n := 0
	for _, ch := range reply {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(s.Delay):
		}
		send(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": string(ch)}}}})
		n++
	}
	finish("stop", n)
}
