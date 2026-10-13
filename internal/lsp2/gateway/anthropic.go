package gateway

// Anthropic Messages compatibility (/v1/messages, docs/protocol-v2.md §6.7).
// A Messages request is translated to the OpenAI chat shape and runs through
// the same pipeline as /v1/chat/completions (routing, signing, blobs, flow,
// resume, Idempotency-Key); only the request translation, the response
// rendering and the error shape differ. The translation is shared verbatim by
// the Node and Python SDKs.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"localsubproxy/internal/lsp2"
)

// anthropicW marks a response as Anthropic-shaped for apiError/httpError/serve.
type anthropicW struct{ http.ResponseWriter }

func (a *anthropicW) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func isAnthropic(w http.ResponseWriter) bool { _, ok := w.(*anthropicW); return ok }

// AnthropicErrorType maps an HTTP status to the Anthropic error type.
func AnthropicErrorType(status int) string {
	switch status {
	case 400, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	}
	return "api_error"
}

func anthropicError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": AnthropicErrorType(status), "message": msg, "lsp2_code": code}})
}

// AnthropicToOpenAI translates a Messages request body into a chat
// completions body. The error is a client error message (400).
func AnthropicToOpenAI(in map[string]any) (map[string]any, error) {
	model, _ := in["model"].(string)
	if model == "" {
		return nil, fmt.Errorf("model: field required")
	}
	mt, ok := in["max_tokens"].(float64)
	if !ok || mt < 1 || mt != float64(int64(mt)) {
		return nil, fmt.Errorf("max_tokens: field required (positive integer)")
	}
	if _, e2ee := in["lsp2_e2ee"]; e2ee {
		return nil, fmt.Errorf("lsp2_e2ee is only supported on /v1/chat/completions")
	}
	out := map[string]any{"model": model, "max_tokens": int64(mt), "stream": in["stream"] == true}
	if t, ok := in["temperature"].(float64); ok {
		out["temperature"] = t
	}
	var msgs []any
	switch s := in["system"].(type) {
	case nil:
	case string:
		if s != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": s})
		}
	case []any:
		var parts []string
		for _, b := range s {
			bm, _ := b.(map[string]any)
			if bm["type"] != "text" {
				return nil, fmt.Errorf("system: only text blocks are supported")
			}
			t, _ := bm["text"].(string)
			parts = append(parts, t)
		}
		msgs = append(msgs, map[string]any{"role": "system", "content": strings.Join(parts, "\n\n")})
	default:
		return nil, fmt.Errorf("system: must be a string or an array of text blocks")
	}
	list, _ := in["messages"].([]any)
	if len(list) == 0 {
		return nil, fmt.Errorf("messages: at least one message is required")
	}
	for i, x := range list {
		m, _ := x.(map[string]any)
		role, _ := m["role"].(string)
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("messages.%d.role: must be user or assistant", i)
		}
		if s, ok := m["content"].(string); ok {
			msgs = append(msgs, map[string]any{"role": role, "content": s})
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			return nil, fmt.Errorf("messages.%d.content: must be a string or an array of blocks", i)
		}
		var parts, calls, results []any
		var text []string
		for j, y := range blocks {
			b, _ := y.(map[string]any)
			switch b["type"] {
			case "text":
				t, _ := b["text"].(string)
				parts = append(parts, map[string]any{"type": "text", "text": t})
				text = append(text, t)
			case "image":
				src, _ := b["source"].(map[string]any)
				var url string
				switch src["type"] {
				case "base64":
					mtp, _ := src["media_type"].(string)
					d, _ := src["data"].(string)
					url = "data:" + mtp + ";base64," + d
				case "url":
					url, _ = src["url"].(string)
				default:
					return nil, fmt.Errorf("messages.%d.content.%d.source.type: base64 or url required", i, j)
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			case "tool_use":
				if role != "assistant" {
					return nil, fmt.Errorf("messages.%d: tool_use blocks belong to assistant messages", i)
				}
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				args, _ := json.Marshal(b["input"])
				if b["input"] == nil {
					args = []byte("{}")
				}
				calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(args)}})
			case "tool_result":
				if role != "user" {
					return nil, fmt.Errorf("messages.%d: tool_result blocks belong to user messages", i)
				}
				id, _ := b["tool_use_id"].(string)
				var content string
				switch c := b["content"].(type) {
				case string:
					content = c
				case []any:
					var ts []string
					for _, z := range c {
						if zm, _ := z.(map[string]any); zm["type"] == "text" {
							t, _ := zm["text"].(string)
							ts = append(ts, t)
						}
					}
					content = strings.Join(ts, "\n")
				}
				if b["is_error"] == true {
					content = "Error: " + content
				}
				results = append(results, map[string]any{"role": "tool", "tool_call_id": id, "content": content})
			case "thinking", "redacted_thinking":
				// not forwarded to local models
			default:
				return nil, fmt.Errorf("messages.%d.content.%d: unsupported block type %v", i, j, b["type"])
			}
		}
		msgs = append(msgs, results...)
		switch {
		case role == "assistant":
			if len(text) == 0 && len(calls) == 0 {
				continue
			}
			am := map[string]any{"role": "assistant", "content": strings.Join(text, "")}
			if len(calls) > 0 {
				am["tool_calls"] = calls
				if len(text) == 0 {
					am["content"] = nil
				}
			}
			msgs = append(msgs, am)
		case len(parts) > 0:
			msgs = append(msgs, map[string]any{"role": "user", "content": parts})
		}
	}
	out["messages"] = msgs
	if tl, ok := in["tools"].([]any); ok && len(tl) > 0 {
		var tools []any
		for i, x := range tl {
			t, _ := x.(map[string]any)
			if ty, has := t["type"]; has && ty != "custom" {
				return nil, fmt.Errorf("tools.%d: server tool %v is not supported", i, ty)
			}
			name, _ := t["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("tools.%d.name: field required", i)
			}
			fn := map[string]any{"name": name, "parameters": t["input_schema"]}
			if d, ok := t["description"].(string); ok {
				fn["description"] = d
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
	}
	if tc, ok := in["tool_choice"].(map[string]any); ok {
		switch tc["type"] {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc["name"]}}
		}
	}
	return out, nil
}

// AnthropicStopReason maps an OpenAI finish_reason.
func AnthropicStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	}
	return "end_turn"
}

func (g *Gateway) messages(w http.ResponseWriter, r *http.Request) {
	aw := &anthropicW{w}
	if r.Method != http.MethodPost {
		anthropicError(aw, 405, lsp2.ErrBadRequest, "POST required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 48<<20+1))
	if err != nil || len(body) > 48<<20 {
		anthropicError(aw, 413, lsp2.ErrPayloadTooLarge, "request body too large")
		return
	}
	var in map[string]any
	if json.Unmarshal(body, &in) != nil || in == nil {
		anthropicError(aw, 400, lsp2.ErrBadRequest, "invalid JSON body")
		return
	}
	out, err := AnthropicToOpenAI(in)
	if err != nil {
		anthropicError(aw, 400, lsp2.ErrBadRequest, err.Error())
		return
	}
	nb, _ := json.Marshal(out)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(nb))
	r2.ContentLength = int64(len(nb))
	g.chat(aw, r2)
}

// serveAnthropic renders a task's events as an Anthropic Messages response.
func (g *Gateway) serveAnthropic(w http.ResponseWriter, r *http.Request, t *gtask, stream bool) {
	msgID := "msg_" + strings.TrimPrefix(t.id, "task_")
	if !stream {
		var text strings.Builder
		var calls []lsp2.ToolCall
		var done *lsp2.TaskDone
		var fail *lsp2.ErrorBody
		if err := t.stream(r.Context(), 0, nil, func(e event) error {
			switch e.Type {
			case lsp2.TypeTaskDelta:
				var d lsp2.TaskDelta
				_ = json.Unmarshal(e.Data, &d)
				text.WriteString(d.Text)
				calls = mergeToolCalls(calls, d.ToolCalls)
			case lsp2.TypeTaskDone:
				done = &lsp2.TaskDone{}
				_ = json.Unmarshal(e.Data, done)
			case lsp2.TypeTaskError:
				fail = &lsp2.ErrorBody{}
				_ = json.Unmarshal(e.Data, fail)
			}
			return nil
		}); err != nil {
			return
		}
		if fail != nil {
			anthropicError(w, errStatus(fail.Code), fail.Code, fail.Message)
			return
		}
		content := []any{}
		if text.Len() > 0 {
			content = append(content, map[string]any{"type": "text", "text": text.String()})
		}
		for _, c := range calls {
			content = append(content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": toolInput(c.Function.Arguments)})
		}
		usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
		if done.Usage != nil {
			usage = map[string]any{"input_tokens": done.Usage.PromptTokens, "output_tokens": done.Usage.CompletionTokens}
		}
		writeJSON(w, 200, map[string]any{"id": msgID, "type": "message", "role": "assistant", "model": t.reqModel, "content": content,
			"stop_reason": AnthropicStopReason(done.FinishReason), "stop_sequence": nil, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	send := func(typ string, v any) error {
		b, _ := json.Marshal(v)
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
		if flusher != nil {
			flusher.Flush()
		}
		return err
	}
	_ = send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": msgID, "type": "message", "role": "assistant",
		"model": t.reqModel, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	idx, open := -1, ""
	tools := map[int]int{} // OpenAI tool index → content block index
	closeOpen := func() {
		if open != "" {
			_ = send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			open = ""
		}
	}
	_ = t.stream(r.Context(), 0, func() error { return send("ping", map[string]any{"type": "ping"}) }, func(e event) error {
		switch e.Type {
		case lsp2.TypeTaskDelta:
			var d lsp2.TaskDelta
			_ = json.Unmarshal(e.Data, &d)
			if d.Text != "" {
				if open != "text" {
					closeOpen()
					idx++
					open = "text"
					_ = send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "text", "text": ""}})
				}
				_ = send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": d.Text}})
			}
			for _, c := range d.ToolCalls {
				ti := 0
				if c.Index != nil {
					ti = *c.Index
				}
				bi, seen := tools[ti]
				if !seen {
					closeOpen()
					idx++
					bi, tools[ti], open = idx, idx, fmt.Sprintf("tool%d", ti)
					id := c.ID
					if id == "" {
						id = fmt.Sprintf("toolu_%s_%d", strings.TrimPrefix(t.id, "task_"), ti)
					}
					_ = send("content_block_start", map[string]any{"type": "content_block_start", "index": bi, "content_block": map[string]any{"type": "tool_use", "id": id, "name": c.Function.Name, "input": map[string]any{}}})
				}
				if c.Function.Arguments != "" {
					_ = send("content_block_delta", map[string]any{"type": "content_block_delta", "index": bi, "delta": map[string]any{"type": "input_json_delta", "partial_json": c.Function.Arguments}})
				}
			}
		case lsp2.TypeTaskDone:
			var d lsp2.TaskDone
			_ = json.Unmarshal(e.Data, &d)
			closeOpen()
			usage := map[string]any{"output_tokens": 0}
			if d.Usage != nil {
				usage = map[string]any{"input_tokens": d.Usage.PromptTokens, "output_tokens": d.Usage.CompletionTokens}
			}
			_ = send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": AnthropicStopReason(d.FinishReason), "stop_sequence": nil}, "usage": usage})
			return send("message_stop", map[string]any{"type": "message_stop"})
		case lsp2.TypeTaskError:
			var eb lsp2.ErrorBody
			_ = json.Unmarshal(e.Data, &eb)
			return send("error", map[string]any{"type": "error", "error": map[string]any{"type": AnthropicErrorType(errStatus(eb.Code)), "message": eb.Message, "lsp2_code": eb.Code}})
		}
		return nil
	})
}

func mergeToolCalls(calls, deltas []lsp2.ToolCall) []lsp2.ToolCall {
	for _, c := range deltas {
		idx := 0
		if c.Index != nil {
			idx = *c.Index
		}
		for len(calls) <= idx {
			calls = append(calls, lsp2.ToolCall{Type: "function"})
		}
		if c.ID != "" {
			calls[idx].ID = c.ID
		}
		calls[idx].Function.Name += c.Function.Name
		calls[idx].Function.Arguments += c.Function.Arguments
	}
	return calls
}

func toolInput(args string) any {
	var v any
	if args == "" || json.Unmarshal([]byte(args), &v) != nil {
		return map[string]any{}
	}
	return v
}

// anthropicModels renders /v1/models in the Anthropic list shape.
func anthropicModels(w http.ResponseWriter, data []map[string]any) {
	out := []any{}
	for _, m := range data {
		out = append(out, map[string]any{"type": "model", "id": m["id"], "display_name": m["id"], "created_at": "1970-01-01T00:00:00Z"})
	}
	var first, last any
	if len(out) > 0 {
		first, last = data[0]["id"], data[len(data)-1]["id"]
	}
	writeJSON(w, 200, map[string]any{"data": out, "has_more": false, "first_id": first, "last_id": last})
}
