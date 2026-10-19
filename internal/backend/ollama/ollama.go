package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/httpapi"
	"github.com/tyro668/avora/internal/lmbp"
)

// Ollama /api/chat mapping (docs/lmbp-spec.md §11.6): text parts are joined
// into "content"; data: images become "images" (raw base64; Ollama cannot
// fetch URLs, so https images are CAPABILITY_UNSUPPORTED); assistant
// tool_calls become {"function":{"name","arguments":<object>}}; role "tool"
// is kept; tools pass through (OpenAI format). Ollama returns whole tool
// calls, which are emitted as single tool_call deltas with generated ids.
func ollamaMessages(msgs []lmbp.Message) ([]map[string]any, error) {
	var out []map[string]any
	for _, m := range msgs {
		om := map[string]any{"role": m.Role, "content": m.Content.PlainText()}
		if m.Role == "developer" {
			om["role"] = "system"
		}
		var images []string
		for _, p := range m.Content.Parts {
			if p.ImageURL != nil {
				_, data, ok := httpapi.ParseDataURI(p.ImageURL.URL)
				if !ok {
					return nil, backend.Failure("CAPABILITY_UNSUPPORTED: Ollama accepts inline (data:) images only")
				}
				images = append(images, data)
			}
			if p.InputAudio != nil {
				return nil, backend.Failure("CAPABILITY_UNSUPPORTED: Ollama does not accept audio")
			}
		}
		if len(images) > 0 {
			om["images"] = images
		}
		if len(m.ToolCalls) > 0 {
			var calls []map[string]any
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Function.Arguments)
				if !json.Valid(args) {
					args = json.RawMessage("{}")
				}
				calls = append(calls, map[string]any{"function": map[string]any{"name": tc.Function.Name, "arguments": args}})
			}
			om["tool_calls"] = calls
		}
		out = append(out, om)
	}
	return out, nil
}

// ollamaTools applies tool_choice with only the semantics Ollama can
// guarantee (it has no tool_choice): "auto"/absent pass the tools, "none"
// withholds them so no call can happen; "required" and a named function
// cannot be enforced and are rejected rather than silently ignored.
func ollamaTools(run lmbp.TaskRun) (any, error) {
	if len(run.ToolChoice) == 0 || string(run.ToolChoice) == "null" {
		if len(run.Tools) == 0 {
			return nil, nil
		}
		return run.Tools, nil
	}
	var choice string
	if json.Unmarshal(run.ToolChoice, &choice) == nil {
		switch choice {
		case "auto":
			if len(run.Tools) == 0 {
				return nil, nil
			}
			return run.Tools, nil
		case "none":
			return nil, nil
		}
	}
	return nil, backend.Failure("CAPABILITY_UNSUPPORTED: Ollama cannot enforce tool_choice " + string(run.ToolChoice) + " (only \"auto\" and \"none\")")
}

func (x *Adapter) callOllama(ctx context.Context, model string, run lmbp.TaskRun, a backend.Config, out *backend.Emitter) (*lmbp.TaskDone, error) {
	msgs, err := ollamaMessages(run.Messages)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"model": model, "messages": msgs, "stream": true}
	tools, err := ollamaTools(run)
	if err != nil {
		return nil, err
	}
	if tools != nil {
		body["tools"] = tools
	}
	opts := map[string]any{}
	if mt := httpapi.MaxTokens(run, a); mt > 0 {
		opts["num_predict"] = mt
	}
	if run.Temperature != nil {
		opts["temperature"] = *run.Temperature
	}
	if run.TopP != nil {
		opts["top_p"] = *run.TopP
	}
	if len(run.Stop) > 0 {
		opts["stop"] = run.Stop
	}
	if len(opts) > 0 {
		body["options"] = opts
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(a.BaseURL, "/")+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := x.modelClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, backend.Failure(fmt.Sprintf("UPSTREAM_ERROR: ollama returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	calls := 0
	for sc.Scan() {
		var part struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Done            bool   `json:"done"`
			DoneReason      string `json:"done_reason"`
			PromptEvalCount int    `json:"prompt_eval_count"`
			EvalCount       int    `json:"eval_count"`
			Error           string `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &part); err != nil {
			return nil, fmt.Errorf("invalid Ollama stream: %w", err)
		}
		if part.Error != "" {
			return nil, backend.Failure("UPSTREAM_ERROR: " + part.Error)
		}
		d := lmbp.TaskDelta{Text: part.Message.Content}
		for _, tc := range part.Message.ToolCalls {
			i := calls
			calls++
			args := string(tc.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			d.ToolCalls = append(d.ToolCalls, lmbp.ToolCall{Index: &i, ID: fmt.Sprintf("call_%d", i), Type: "function", Function: lmbp.FunctionCall{Name: tc.Function.Name, Arguments: args}})
		}
		if err := out.Delta(d); err != nil {
			return nil, err
		}
		if part.Done {
			reason := "stop"
			switch {
			case calls > 0:
				reason = "tool_calls"
			case part.DoneReason == "length":
				reason = "length"
			}
			return &lmbp.TaskDone{FinishReason: reason, Usage: &lmbp.Usage{PromptTokens: part.PromptEvalCount, CompletionTokens: part.EvalCount}}, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("Ollama stream ended without done marker")
}
