package conformance

// A*: Anthropic Messages compatibility (/v1/messages, LMBP spec §6.7).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

func (r *runner) messagesRaw(ctx context.Context, body any, hdr map[string]string) (int, http.Header, []byte) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.GatewayURL+"/v1/messages", bytes.NewReader(b))
	req.Header.Set("X-Api-Key", r.cfg.TenantKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-End-User", r.cfg.EndUser)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
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

type namedEvent struct {
	event, id string
	data      map[string]any
}

func parseNamedSSE(b []byte) []namedEvent {
	var out []namedEvent
	var cur namedEvent
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "event: "):
			cur.event = l[7:]
		case strings.HasPrefix(l, "id: "):
			cur.id = l[4:]
		case strings.HasPrefix(l, "data: "):
			_ = json.Unmarshal([]byte(l[6:]), &cur.data)
		case l == "":
			if cur.event != "" {
				out = append(out, cur)
			}
			cur = namedEvent{}
		}
	}
	return out
}

func anthErrType(b []byte) string {
	var m struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &m)
	if m.Type != "error" {
		return ""
	}
	return m.Error.Type
}

func (r *runner) runAnthropic(ctx context.Context, dev *extDev, full func(string) string, mTools string) {
	type res struct {
		st   int
		h    http.Header
		body []byte
	}
	call := func(body any, hdr map[string]string) chan res {
		ch := make(chan res, 1)
		go func() { s, h, b := r.messagesRaw(ctx, body, hdr); ch <- res{s, h, b} }()
		return ch
	}
	run := func() (lmbp.TaskRun, error) {
		f, _, err := dev.await(lmbp.TypeTaskRun, nil)
		if err != nil {
			return lmbp.TaskRun{}, err
		}
		return lmbp.VerifyTaskRun(r.gwKey, f.TaskID, f.JWS)
	}
	usage := &lmbp.Usage{PromptTokens: 7, CompletionTokens: 3}

	// A1 non-streaming text with a system prompt
	ch := call(map[string]any{"model": full(mTools), "max_tokens": 64, "system": "be brief", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
	tr, err := run()
	sysOK := err == nil && len(tr.Messages) == 2 && tr.Messages[0].Role == "system" && tr.Messages[0].Content.Text == "be brief" && tr.Messages[1].Content.Text == "hello"
	r.check("A1", "/v1/messages maps system and text into the signed task.run", want(sysOK, "err %v run %+v", err, tr.Messages))
	if err == nil {
		_ = dev.send(lmbp.TypeTaskDelta, tr.TaskID, 1, lmbp.TaskDelta{Text: "hi "})
		_ = dev.send(lmbp.TypeTaskDelta, tr.TaskID, 2, lmbp.TaskDelta{Text: "there"})
		_ = dev.send(lmbp.TypeTaskDone, tr.TaskID, 3, lmbp.TaskDone{FinishReason: "stop", Usage: usage})
		rs := <-ch
		var m struct {
			ID, Type, Role, StopReason string
			Content                    []map[string]any
			Usage                      map[string]float64
		}
		_ = json.Unmarshal(rs.body, &m)
		var raw map[string]any
		_ = json.Unmarshal(rs.body, &raw)
		ok := rs.st == 200 && strings.HasPrefix(m.ID, "msg_") && m.Type == "message" && m.Role == "assistant" && raw["stop_reason"] == "end_turn" &&
			len(m.Content) == 1 && m.Content[0]["type"] == "text" && m.Content[0]["text"] == "hi there" && m.Usage["input_tokens"] == 7 && m.Usage["output_tokens"] == 3
		r.check("A2", "non-streaming /v1/messages returns an Anthropic message with text content, stop_reason end_turn and usage", want(ok, "status %d body %s", rs.st, rs.body))
	}

	// A3 streaming: text then tool_use, event order
	tools := []any{map[string]any{"name": "get_weather", "description": "w", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]string{"type": "string"}}}}}
	idem := "conf-anth-" + randHex()
	streamBody := map[string]any{"model": full(mTools), "max_tokens": 64, "stream": true, "tools": tools, "tool_choice": map[string]any{"type": "any"},
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "weather in Paris?"}}}}}
	ch = call(streamBody, map[string]string{"Idempotency-Key": idem})
	tr, err = run()
	r.check("A3", "tools (input_schema) and tool_choice any are carried as OpenAI function tools / required", want(err == nil && len(tr.Tools) == 1 && tr.Tools[0].Function.Name == "get_weather" &&
		tr.Tools[0].Function.Parameters != nil && string(mustJSON(tr.ToolChoice)) == `"required"`, "err %v tools %+v choice %s", err, tr.Tools, mustJSON(tr.ToolChoice)))
	if err == nil {
		idx := 0
		_ = dev.send(lmbp.TypeTaskDelta, tr.TaskID, 1, lmbp.TaskDelta{Text: "Checking."})
		_ = dev.send(lmbp.TypeTaskDelta, tr.TaskID, 2, lmbp.TaskDelta{ToolCalls: []lmbp.ToolCall{{Index: &idx, ID: "call_9", Type: "function", Function: lmbp.FunctionCall{Name: "get_weather", Arguments: `{"city":`}}}})
		_ = dev.send(lmbp.TypeTaskDelta, tr.TaskID, 3, lmbp.TaskDelta{ToolCalls: []lmbp.ToolCall{{Index: &idx, Function: lmbp.FunctionCall{Arguments: `"Paris"}`}}}})
		_ = dev.send(lmbp.TypeTaskDone, tr.TaskID, 4, lmbp.TaskDone{FinishReason: "tool_calls", Usage: usage})
		rs := <-ch
		evs := parseNamedSSE(rs.body)
		var names []string
		var js strings.Builder
		var toolStart map[string]any
		var stop any
		for _, e := range evs {
			names = append(names, e.event)
			if e.event == "content_block_start" {
				if cb, _ := e.data["content_block"].(map[string]any); cb["type"] == "tool_use" {
					toolStart = cb
				}
			}
			if d, _ := e.data["delta"].(map[string]any); e.event == "content_block_delta" && d["type"] == "input_json_delta" {
				pj, _ := d["partial_json"].(string)
				js.WriteString(pj)
			}
			if d, _ := e.data["delta"].(map[string]any); e.event == "message_delta" {
				stop = d["stop_reason"]
			}
		}
		seq := strings.Join(names, ",")
		wantSeq := "message_start,content_block_start,content_block_delta,content_block_stop,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
		r.check("A4", "streaming /v1/messages emits message_start, text and tool_use content blocks (text_delta/input_json_delta), message_delta (stop_reason tool_use) and message_stop in order",
			want(rs.st == 200 && strings.HasPrefix(rs.h.Get("Content-Type"), "text/event-stream") && seq == wantSeq && toolStart["name"] == "get_weather" && toolStart["id"] == "call_9" &&
				js.String() == `{"city":"Paris"}` && stop == "tool_use", "status %d events %s json %q start %v", rs.st, seq, js.String(), toolStart))
		// A5 reattach with Idempotency-Key + Last-Event-ID resumes after that event
		var lastText string
		for _, e := range evs {
			if d, _ := e.data["delta"].(map[string]any); e.id != "" && d["type"] == "input_json_delta" {
				lastText = e.id
				break
			}
		}
		s5, _, b5 := r.messagesRaw(ctx, streamBody, map[string]string{"Idempotency-Key": idem, "Last-Event-ID": lastText})
		evs5 := parseNamedSSE(b5)
		var n5 []string
		for _, e := range evs5 {
			n5 = append(n5, e.event)
		}
		r.check("A5", "Idempotency-Key replay with Last-Event-ID resumes the Anthropic stream after that event without repeating message_start",
			want(lastText != "" && s5 == 200 && len(n5) > 0 && n5[0] == "content_block_delta" && n5[len(n5)-1] == "message_stop", "last %q status %d events %v", lastText, s5, n5))
	}

	// A6 tool_use / tool_result history mapping, and image block → blob
	img := make([]byte, 40<<10)
	_, _ = rand.Read(img)
	ch = call(map[string]any{"model": full(mTools), "max_tokens": 64, "tools": tools, "messages": []any{
		map[string]any{"role": "user", "content": "weather?"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]any{"city": "Paris"}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(img)}}}},
	}}, nil)
	tr, err = run()
	if err == nil {
		dev.blobs(tr)
	}
	ms := tr.Messages
	ok := err == nil && len(ms) == 4 && len(ms[1].ToolCalls) == 1 && ms[1].ToolCalls[0].ID == "toolu_1" && ms[1].ToolCalls[0].Function.Arguments == `{"city":"Paris"}` &&
		ms[2].Role == "tool" && ms[2].ToolCallID == "toolu_1" && ms[2].Content.Text == "sunny" && len(ms[3].Content.Parts) == 1 &&
		len(tr.Blobs) == 1 && tr.Blobs[0].Size == int64(len(img)) && ms[3].Content.Parts[0].ImageURL != nil && ms[3].Content.Parts[0].ImageURL.URL == lmbp.BlobScheme+tr.Blobs[0].ID
	r.check("A6", "assistant tool_use → tool_calls, user tool_result → role tool, base64 image block → blob-backed image part", want(ok, "err %v messages %s blobs %+v", err, mustJSON(ms), tr.Blobs))
	if err == nil {
		_ = dev.send(lmbp.TypeTaskDone, tr.TaskID, 1, lmbp.TaskDone{FinishReason: "stop"})
		<-ch
	}

	// A7..A9 errors and models
	s, _, b := r.messagesRaw(ctx, map[string]any{"model": full(mTools), "messages": []any{map[string]any{"role": "user", "content": "x"}}}, nil)
	r.check("A7", "missing max_tokens fails with 400 invalid_request_error in the Anthropic error shape", want(s == 400 && anthErrType(b) == "invalid_request_error", "status %d %s", s, b))
	s, _, b = r.messagesRaw(ctx, map[string]any{"model": full(mTools), "max_tokens": 8, "messages": []any{map[string]any{"role": "user", "content": "x"}}}, map[string]string{"X-Api-Key": "wrong"})
	r.check("A8", "a bad x-api-key fails with 401 authentication_error", want(s == 401 && anthErrType(b) == "authentication_error", "status %d %s", s, b))
	s, _, b = r.messagesRaw(ctx, map[string]any{"model": "no-such-device/none", "max_tokens": 8, "messages": []any{map[string]any{"role": "user", "content": "x"}}}, nil)
	r.check("A9", "an unknown model fails with a 4xx Anthropic error (not_found_error)", want(s == 404 && anthErrType(b) == "not_found_error", "status %d %s", s, b))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.GatewayURL+"/v1/models", nil)
	req.Header.Set("X-Api-Key", r.cfg.TenantKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-End-User", r.cfg.EndUser)
	var ml struct {
		Data    []map[string]any `json:"data"`
		HasMore *bool            `json:"has_more"`
	}
	st := 0
	if resp, err := http.DefaultClient.Do(req); err == nil {
		st = resp.StatusCode
		_ = json.NewDecoder(resp.Body).Decode(&ml)
		resp.Body.Close()
	}
	found := false
	for _, m := range ml.Data {
		if m["id"] == full(mTools) && m["type"] == "model" && m["display_name"] != nil {
			found = true
		}
	}
	r.check("A10", "GET /v1/models with anthropic-version returns the Anthropic list shape", want(st == 200 && found && ml.HasMore != nil, "status %d %+v", st, ml))
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return strings.ToLower(base64.RawURLEncoding.EncodeToString(b))
}
