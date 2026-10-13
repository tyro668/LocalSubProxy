package connector

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

	"localsubproxy/internal/lsp2"
)

// Ollama /api/chat mapping (docs/protocol-v2.md §11.6): text parts are joined
// into "content"; data: images become "images" (raw base64; Ollama cannot
// fetch URLs, so https images are CAPABILITY_UNSUPPORTED); assistant
// tool_calls become {"function":{"name","arguments":<object>}}; role "tool"
// is kept; tools pass through (OpenAI format). Ollama returns whole tool
// calls, which are emitted as single tool_call deltas with generated ids.
func ollamaMessages(msgs []lsp2.Message) ([]map[string]any, error) {
	var out []map[string]any
	for _, m := range msgs {
		om := map[string]any{"role": m.Role, "content": m.Content.PlainText()}
		if m.Role == "developer" {
			om["role"] = "system"
		}
		var images []string
		for _, p := range m.Content.Parts {
			if p.ImageURL != nil {
				_, data, ok := parseDataURI(p.ImageURL.URL)
				if !ok {
					return nil, knownFailure("CAPABILITY_UNSUPPORTED: Ollama accepts inline (data:) images only")
				}
				images = append(images, data)
			}
			if p.InputAudio != nil {
				return nil, knownFailure("CAPABILITY_UNSUPPORTED: Ollama does not accept audio")
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

func (x *Connector) callOllama(ctx context.Context, model string, run lsp2.TaskRun, a AdapterConfig, out *limitedEmitter) (*lsp2.TaskDone, error) {
	msgs, err := ollamaMessages(run.Messages)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"model": model, "messages": msgs, "stream": true}
	if len(run.Tools) > 0 {
		body["tools"] = run.Tools
	}
	opts := map[string]any{}
	if mt := maxTokens(run, a); mt > 0 {
		opts["num_predict"] = mt
	}
	if run.Temperature != nil {
		opts["temperature"] = *run.Temperature
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
		return nil, knownFailure(fmt.Sprintf("UPSTREAM_ERROR: ollama returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
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
			return nil, knownFailure("UPSTREAM_ERROR: " + part.Error)
		}
		d := lsp2.TaskDelta{Text: part.Message.Content}
		for _, tc := range part.Message.ToolCalls {
			i := calls
			calls++
			args := string(tc.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			d.ToolCalls = append(d.ToolCalls, lsp2.ToolCall{Index: &i, ID: fmt.Sprintf("call_%d", i), Type: "function", Function: lsp2.FunctionCall{Name: tc.Function.Name, Arguments: args}})
		}
		if err := out.delta(d); err != nil {
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
			return &lsp2.TaskDone{FinishReason: reason, Usage: &lsp2.Usage{PromptTokens: part.PromptEvalCount, CompletionTokens: part.EvalCount}}, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("Ollama stream ended without done marker")
}
