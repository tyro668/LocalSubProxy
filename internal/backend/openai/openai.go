package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/httpapi"
	"github.com/tyro668/avora/internal/lmbp"
	"github.com/tyro668/avora/internal/lmbp/client"
)

// callOpenAI forwards the request to an OpenAI-compatible Chat Completions
// endpoint. lmbp messages, tools, tool_choice and content parts are the
// OpenAI shapes, so they pass through unchanged. The configured key never
// enters the protocol, health reports or logs.
func (x *Adapter) callOpenAI(ctx context.Context, model string, run lmbp.TaskRun, a backend.Config, out *backend.Emitter) (*lmbp.TaskDone, error) {
	payload := map[string]any{"model": model, "messages": run.Messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	if len(run.Tools) > 0 {
		payload["tools"] = run.Tools
	}
	if len(run.ToolChoice) > 0 {
		payload["tool_choice"] = run.ToolChoice
	}
	if mt := httpapi.MaxTokens(run, a); mt > 0 {
		payload["max_tokens"] = mt
	}
	if run.Temperature != nil {
		payload["temperature"] = *run.Temperature
	}
	if run.TopP != nil {
		payload["top_p"] = *run.TopP
	}
	if len(run.Stop) > 0 {
		payload["stop"] = run.Stop
	}
	if a.ReasoningEffort != "" && (a.ReasoningEffort != "off" || a.Provider == "deepseek") {
		effort := a.ReasoningEffort
		if effort == "off" {
			effort = "none"
		}
		payload["reasoning_effort"] = effort
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpapi.Endpoint(a, "/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if a.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	}
	res, err := x.modelClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if err := httpapi.Status(res.StatusCode); err != nil {
		return nil, err
	}
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		var completion struct {
			Choices []struct {
				Message struct {
					Content   string          `json:"content"`
					ToolCalls []lmbp.ToolCall `json:"tool_calls"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *lmbp.Usage `json:"usage"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, backend.MaxOutputBytes+1)).Decode(&completion); err != nil {
			return nil, err
		}
		if len(completion.Choices) == 0 {
			return nil, errors.New("provider returned no choices")
		}
		ch := completion.Choices[0]
		for i := range ch.Message.ToolCalls {
			idx := i
			ch.Message.ToolCalls[i].Index = &idx
		}
		if err := out.Delta(lmbp.TaskDelta{Text: ch.Message.Content, ToolCalls: ch.Message.ToolCalls}); err != nil {
			return nil, err
		}
		if ch.FinishReason == "" {
			ch.FinishReason = "stop"
		}
		return &lmbp.TaskDone{FinishReason: ch.FinishReason, Usage: completion.Usage}, nil
	}
	var emitErr error
	done, fail := client.RelayOpenAIStream(ctx, res.Body, func(d lmbp.TaskDelta) error {
		emitErr = out.Delta(d)
		return emitErr
	})
	if emitErr != nil {
		return nil, emitErr
	}
	if fail != nil {
		if fail.Code == lmbp.ErrCancelled {
			return nil, context.Canceled
		}
		return nil, backend.Failure("UPSTREAM_ERROR: " + fail.Message)
	}
	return done, nil
}
