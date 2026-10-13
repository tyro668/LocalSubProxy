package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/tyro668/LocalSubProxy/internal/lsp2"
)

// Anthropic Messages mapping (docs/protocol-v2.md §11.6):
//   - system messages → top-level "system";
//   - text parts → {"type":"text"}; image_url data: → {"type":"image","source":{"type":"base64"}},
//     https → {"type":"image","source":{"type":"url"}};
//   - assistant tool_calls → {"type":"tool_use","id","name","input"};
//   - role "tool" → user {"type":"tool_result","tool_use_id","content"};
//   - tools → {"name","description","input_schema"}; tool_choice auto/required/none/function →
//     {"type":"auto"|"any"|"none"|"tool"};
//   - stream: tool_use blocks → tool_call deltas; stop_reason end_turn/tool_use/max_tokens →
//     stop/tool_calls/length.

func anthropicContent(c lsp2.Content) ([]map[string]any, error) {
	if c.Parts == nil {
		if c.Text == "" {
			return nil, nil
		}
		return []map[string]any{{"type": "text", "text": c.Text}}, nil
	}
	var out []map[string]any
	for _, p := range c.Parts {
		switch {
		case p.Type == "text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case p.ImageURL != nil:
			if mt, data, ok := parseDataURI(p.ImageURL.URL); ok {
				out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mt, "data": data}})
			} else {
				out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": p.ImageURL.URL}})
			}
		default:
			return nil, knownFailure("CAPABILITY_UNSUPPORTED: Anthropic adapter does not accept " + p.Type)
		}
	}
	return out, nil
}

func anthropicRequest(model string, run lsp2.TaskRun, a AdapterConfig) (map[string]any, error) {
	var system []string
	var messages []map[string]any
	add := func(role string, blocks []map[string]any) {
		if n := len(messages); n > 0 && messages[n-1]["role"] == role {
			messages[n-1]["content"] = append(messages[n-1]["content"].([]map[string]any), blocks...)
			return
		}
		messages = append(messages, map[string]any{"role": role, "content": blocks})
	}
	for _, m := range run.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, m.Content.PlainText())
		case "tool":
			add("user", []map[string]any{{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content.PlainText()}})
		default:
			blocks, err := anthropicContent(m.Content)
			if err != nil {
				return nil, err
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if !json.Valid(input) {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
			}
			if len(blocks) > 0 {
				add(m.Role, blocks)
			}
		}
	}
	if len(messages) == 0 {
		return nil, knownFailure("POLICY_DENIED: Anthropic Messages requires a user or assistant message")
	}
	mt := maxTokens(run, a)
	if mt == 0 {
		mt = 1024 // Messages requires max_tokens.
	}
	payload := map[string]any{"model": model, "messages": messages, "max_tokens": mt, "stream": true}
	if len(system) > 0 {
		payload["system"] = strings.Join(system, "\n\n")
	}
	if run.Temperature != nil {
		payload["temperature"] = *run.Temperature
	}
	if len(run.Tools) > 0 {
		var tools []map[string]any
		for _, t := range run.Tools {
			schema := t.Function.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{"name": t.Function.Name, "description": t.Function.Description, "input_schema": schema})
		}
		payload["tools"] = tools
	}
	if len(run.ToolChoice) > 0 {
		var s string
		var obj struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		switch {
		case json.Unmarshal(run.ToolChoice, &s) == nil:
			payload["tool_choice"] = map[string]map[string]string{"auto": {"type": "auto"}, "required": {"type": "any"}, "none": {"type": "none"}}[s]
		case json.Unmarshal(run.ToolChoice, &obj) == nil && obj.Function.Name != "":
			payload["tool_choice"] = map[string]string{"type": "tool", "name": obj.Function.Name}
		}
	}
	return payload, nil
}

func (x *Connector) callAnthropic(ctx context.Context, model string, run lsp2.TaskRun, a AdapterConfig, out *limitedEmitter) (*lsp2.TaskDone, error) {
	payload, err := anthropicRequest(model, run, a)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, adapterEndpoint(a, "/messages"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	res, err := x.modelClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if err := providerStatus(res.StatusCode); err != nil {
		return nil, err
	}
	finish := func(stop string) string {
		switch stop {
		case "tool_use":
			return "tool_calls"
		case "max_tokens":
			return "length"
		}
		return "stop"
	}
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		var msg struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
			Usage      struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, maxOutputBytes+1)).Decode(&msg); err != nil {
			return nil, err
		}
		idx := 0
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				err = out.text(b.Text)
			case "tool_use":
				i := idx
				idx++
				err = out.delta(lsp2.TaskDelta{ToolCalls: []lsp2.ToolCall{{Index: &i, ID: b.ID, Type: "function", Function: lsp2.FunctionCall{Name: b.Name, Arguments: string(b.Input)}}}})
			}
			if err != nil {
				return nil, err
			}
		}
		return &lsp2.TaskDone{FinishReason: finish(msg.StopReason), Usage: &lsp2.Usage{PromptTokens: msg.Usage.InputTokens, CompletionTokens: msg.Usage.OutputTokens}}, nil
	}
	done := &lsp2.TaskDone{FinishReason: "stop", Usage: &lsp2.Usage{}}
	toolIndex := map[int]int{} // content block index → tool call index
	stopped := false
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var data []string
	consume := func() error {
		if len(data) == 0 {
			return nil
		}
		defer func() { data = nil }()
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &ev); err != nil {
			return errors.New("invalid Anthropic SSE JSON")
		}
		switch ev.Type {
		case "message_start":
			done.Usage.PromptTokens = ev.Message.Usage.InputTokens
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				i := len(toolIndex)
				toolIndex[ev.Index] = i
				return out.delta(lsp2.TaskDelta{ToolCalls: []lsp2.ToolCall{{Index: &i, ID: ev.ContentBlock.ID, Type: "function", Function: lsp2.FunctionCall{Name: ev.ContentBlock.Name}}}})
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				return out.text(ev.Delta.Text)
			case "input_json_delta":
				if i, ok := toolIndex[ev.Index]; ok && ev.Delta.PartialJSON != "" {
					return out.delta(lsp2.TaskDelta{ToolCalls: []lsp2.ToolCall{{Index: &i, Function: lsp2.FunctionCall{Arguments: ev.Delta.PartialJSON}}}})
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				done.FinishReason = finish(ev.Delta.StopReason)
			}
			done.Usage.CompletionTokens = ev.Usage.OutputTokens
		case "message_stop":
			stopped = true
		case "error":
			return knownFailure("UPSTREAM_ERROR: provider stream error")
		}
		return nil
	}
	for sc.Scan() && !stopped {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if err := consume(); err != nil {
				return nil, err
			}
			continue
		}
		if p, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(p))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !stopped {
		if err := consume(); err != nil {
			return nil, err
		}
	}
	if !stopped {
		return nil, errors.New("provider stream ended without message_stop")
	}
	return done, nil
}
