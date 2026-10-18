package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

// Adapter mappings for tools and multimodal input against fake providers.

var weather = []lmbp.Tool{{Type: "function", Function: lmbp.FunctionDef{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}}}

func imageMsgs(url string) []lmbp.Message {
	return []lmbp.Message{{Role: "system", Content: lmbp.Text("be brief")}, {Role: "user", Content: lmbp.Content{Parts: []lmbp.ContentPart{{Type: "text", Text: "what?"}, {Type: "image_url", ImageURL: &lmbp.ImageURL{URL: url}}}}}}
}

func toolRound() []lmbp.Message {
	return []lmbp.Message{
		{Role: "user", Content: lmbp.Text("weather?")},
		{Role: "assistant", Content: lmbp.Content{Null: true}, ToolCalls: []lmbp.ToolCall{{ID: "call_1", Type: "function", Function: lmbp.FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: lmbp.Text("18C")},
	}
}

type collected struct {
	text  strings.Builder
	name  string
	args  strings.Builder
	calls int
}

func (c *collected) emit(d lmbp.TaskDelta) error {
	c.text.WriteString(d.Text)
	for _, tc := range d.ToolCalls {
		if tc.ID != "" {
			c.calls++
		}
		c.name += tc.Function.Name
		c.args.WriteString(tc.Function.Arguments)
	}
	return nil
}

func newX(t *testing.T, a AdapterConfig) *Connector {
	t.Helper()
	a.Enabled = true
	x, err := New(Config{Adapters: []AdapterConfig{a}})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func run(model string, msgs []lmbp.Message, tools []lmbp.Tool) lmbp.TaskRun {
	return lmbp.TaskRun{TaskID: lmbp.RandomID("t_"), Model: model, Messages: msgs, Tools: tools}
}

func TestAnthropicMapping(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(typ, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data) }
		ev("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":12}}}`)
		if _, ok := got["tools"]; ok && len(got["messages"].([]any)) == 1 {
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`)
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`)
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`)
			ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`)
		} else {
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)
			ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)
		}
		ev("message_stop", `{"type":"message_stop"}`)
	}))
	defer srv.Close()
	x := newX(t, AdapterConfig{ID: "an", Type: "anthropic_compatible", BaseURL: srv.URL, APIKey: "k", Models: []string{"claude"}})
	ctx := context.Background()
	var c collected
	done, fail := x.Execute(ctx, run("an/claude", []lmbp.Message{{Role: "user", Content: lmbp.Text("weather?")}}, weather), c.emit)
	if fail != nil || done.FinishReason != "tool_calls" || c.name != "get_weather" || c.args.String() != `{"city":"Paris"}` || done.Usage.PromptTokens != 12 {
		t.Fatalf("anthropic tool_use: %v %v %q %q", done, fail, c.name, c.args.String())
	}
	if got["tools"].([]any)[0].(map[string]any)["input_schema"] == nil {
		t.Fatalf("tools not mapped: %v", got["tools"])
	}
	c = collected{}
	if _, fail := x.Execute(ctx, run("an/claude", toolRound(), weather), c.emit); fail != nil || c.text.String() != "ok" {
		t.Fatalf("anthropic tool result: %v", fail)
	}
	msgs := got["messages"].([]any)
	asst := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if asst["type"] != "tool_use" || asst["input"].(map[string]any)["city"] != "Paris" || res["type"] != "tool_result" || res["tool_use_id"] != "call_1" {
		t.Fatalf("tool messages mapping: %v", msgs)
	}
	if _, fail := x.Execute(ctx, run("an/claude", imageMsgs("data:image/png;base64,iVBORw0KGgo="), nil), c.emit); fail != nil {
		t.Fatal(fail)
	}
	img := got["messages"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)
	if got["system"] != "be brief" || img["type"] != "image" || img["source"].(map[string]any)["media_type"] != "image/png" || img["source"].(map[string]any)["data"] != "iVBORw0KGgo=" {
		t.Fatalf("image mapping: %v", got)
	}
}

func TestOllamaMapping(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			fmt.Fprint(w, `{"models":[{"name":"llava"}]}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
		if _, ok := got["tools"]; ok && len(got["messages"].([]any)) == 1 {
			fmt.Fprintln(w, `{"message":{"content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"done":false}`)
			fmt.Fprintln(w, `{"message":{"content":""},"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":3}`)
			return
		}
		fmt.Fprintln(w, `{"message":{"content":"seen"},"done":false}`)
		fmt.Fprintln(w, `{"message":{"content":""},"done":true,"prompt_eval_count":5,"eval_count":1}`)
	}))
	defer srv.Close()
	x := newX(t, AdapterConfig{ID: "ol", Type: "ollama_http", BaseURL: srv.URL, Models: []string{"llava"}, Capabilities: []string{"tools", "vision"}})
	ctx := context.Background()
	var c collected
	done, fail := x.Execute(ctx, run("ol/llava", []lmbp.Message{{Role: "user", Content: lmbp.Text("weather?")}}, weather), c.emit)
	if fail != nil || done.FinishReason != "tool_calls" || c.name != "get_weather" || c.args.String() != `{"city":"Paris"}` || c.calls != 1 {
		t.Fatalf("ollama tool call: %v %v %q", done, fail, c.args.String())
	}
	c = collected{}
	if _, fail := x.Execute(ctx, run("ol/llava", toolRound(), weather), c.emit); fail != nil {
		t.Fatal(fail)
	}
	if tc := got["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any); tc["arguments"].(map[string]any)["city"] != "Paris" {
		t.Fatalf("ollama assistant tool_calls: %v", got["messages"])
	}
	if _, fail := x.Execute(ctx, run("ol/llava", imageMsgs("data:image/png;base64,iVBORw0KGgo="), nil), c.emit); fail != nil {
		t.Fatal(fail)
	}
	if imgs := got["messages"].([]any)[1].(map[string]any)["images"].([]any); imgs[0] != "iVBORw0KGgo=" {
		t.Fatalf("ollama images: %v", got)
	}
	if _, fail := x.Execute(ctx, run("ol/llava", imageMsgs("https://example.com/a.png"), nil), c.emit); fail == nil || fail.Code != lmbp.ErrUnsupported {
		t.Fatalf("https image on ollama: %v", fail)
	}
}

func TestOpenAIPassthroughAndCapabilities(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	x := newX(t, AdapterConfig{ID: "oa", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt"}, SkipModelProbe: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var c collected
	r := run("oa/gpt", imageMsgs("https://example.com/cat.png"), weather)
	r.ToolChoice = json.RawMessage(`"required"`)
	done, fail := x.Execute(ctx, r, c.emit)
	if fail != nil || done.FinishReason != "tool_calls" || c.args.String() != `{"city":"Paris"}` || done.Usage.CompletionTokens != 4 {
		t.Fatalf("openai: %v %v", done, fail)
	}
	if got["tool_choice"] != "required" || got["messages"].([]any)[1].(map[string]any)["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)["url"] != "https://example.com/cat.png" {
		t.Fatalf("openai passthrough: %v", got)
	}
	// Explicit flags restrict capabilities.
	y := newX(t, AdapterConfig{ID: "oa", Type: "openai_compatible", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt"}, Capabilities: []string{"tools"}})
	if _, fail := y.Execute(ctx, run("oa/gpt", imageMsgs("https://example.com/cat.png"), nil), c.emit); fail == nil || fail.Code != lmbp.ErrUnsupported {
		t.Fatalf("vision without flag: %v", fail)
	}
	if caps := y.ModelCapabilities()["oa/gpt"]; !has(caps, "tools") || has(caps, "vision") || has(caps, "audio") || !has(caps, lmbp.ModelCapParams) {
		t.Fatalf("caps: %v (want tools + the param table, no vision/audio)", caps)
	}
}
