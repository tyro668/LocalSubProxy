package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// OpenAIExecutor runs tasks against a local OpenAI-compatible endpoint
// (LM Studio, Ollama /v1, llama.cpp server). Tools, tool messages and
// image/audio content parts are forwarded unchanged; streamed tool_call
// deltas are relayed as task.delta tool_calls.
type OpenAIExecutor struct {
	BaseURL   string
	ModelList []string
	// Caps holds per-model capability flags (tools, vision, audio).
	Caps map[string][]string
}

func (o *OpenAIExecutor) Models() []string { return o.ModelList }

func (o *OpenAIExecutor) ModelCapabilities() map[string][]string { return o.Caps }

func (o *OpenAIExecutor) Execute(ctx context.Context, run lmbp.TaskRun, emit func(lmbp.TaskDelta) error) (*lmbp.TaskDone, *lmbp.ErrorBody) {
	body := map[string]any{"model": run.Model, "messages": run.Messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	if len(run.Tools) > 0 {
		body["tools"] = run.Tools
	}
	if len(run.ToolChoice) > 0 {
		body["tool_choice"] = run.ToolChoice
	}
	if run.MaxTokens > 0 {
		body["max_tokens"] = run.MaxTokens
	}
	if run.Temperature != nil {
		body["temperature"] = *run.Temperature
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.BaseURL, "/")+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &lmbp.ErrorBody{Code: lmbp.ErrCancelled, Message: "cancelled"}
		}
		return nil, &lmbp.ErrorBody{Code: lmbp.ErrUpstream, Message: "local model unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, &lmbp.ErrorBody{Code: lmbp.ErrUpstream, Message: fmt.Sprintf("local model returned HTTP %d", resp.StatusCode)}
	}
	return RelayOpenAIStream(ctx, resp.Body, emit)
}

// RelayOpenAIStream converts an OpenAI chat.completion.chunk SSE stream into
// task.delta emissions and the final task.done.
func RelayOpenAIStream(ctx context.Context, r io.Reader, emit func(lmbp.TaskDelta) error) (*lmbp.TaskDone, *lmbp.ErrorBody) {
	done := &lmbp.TaskDone{FinishReason: "stop"}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		p, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if p == "[DONE]" {
			break
		}
		var ch struct {
			Choices []struct {
				Delta struct {
					Content   string          `json:"content"`
					ToolCalls []lmbp.ToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *lmbp.Usage `json:"usage"`
		}
		if json.Unmarshal([]byte(p), &ch) != nil {
			continue
		}
		if ch.Usage != nil {
			done.Usage = ch.Usage
		}
		for _, x := range ch.Choices {
			if x.Delta.Content != "" || len(x.Delta.ToolCalls) > 0 {
				if emit(lmbp.TaskDelta{Text: x.Delta.Content, ToolCalls: x.Delta.ToolCalls}) != nil {
					return nil, &lmbp.ErrorBody{Code: lmbp.ErrDeviceOffline, Message: "session lost"}
				}
			}
			if x.FinishReason != nil && *x.FinishReason != "" {
				done.FinishReason = *x.FinishReason
			}
		}
	}
	if ctx.Err() != nil {
		return nil, &lmbp.ErrorBody{Code: lmbp.ErrCancelled, Message: "cancelled"}
	}
	if sc.Err() != nil {
		return nil, &lmbp.ErrorBody{Code: lmbp.ErrUpstream, Message: "stream interrupted"}
	}
	return done, nil
}
