package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
)

func providerStatus(code int) error {
	switch code {
	case 200:
		return nil
	case 401, 403:
		return knownFailure("AUTH_REQUIRED")
	case 429:
		return knownFailure("PROVIDER_RATE_LIMITED")
	}
	return knownFailure(fmt.Sprintf("UPSTREAM_ERROR: provider HTTP %d", code))
}

// callOpenAI forwards the request to an OpenAI-compatible Chat Completions
// endpoint. lmbp messages, tools, tool_choice and content parts are the
// OpenAI shapes, so they pass through unchanged. The configured key never
// enters the protocol, health reports or logs.
func (x *Connector) callOpenAI(ctx context.Context, model string, run lmbp.TaskRun, a AdapterConfig, out *limitedEmitter) (*lmbp.TaskDone, error) {
	payload := map[string]any{"model": model, "messages": run.Messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	if len(run.Tools) > 0 {
		payload["tools"] = run.Tools
	}
	if len(run.ToolChoice) > 0 {
		payload["tool_choice"] = run.ToolChoice
	}
	if mt := maxTokens(run, a); mt > 0 {
		payload["max_tokens"] = mt
	}
	if run.Temperature != nil {
		payload["temperature"] = *run.Temperature
	}
	if a.ReasoningEffort != "" && (a.ReasoningEffort != "off" || a.Provider == "deepseek") {
		effort := a.ReasoningEffort
		if effort == "off" {
			effort = "none"
		}
		payload["reasoning_effort"] = effort
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, adapterEndpoint(a, "/chat/completions"), bytes.NewReader(body))
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
	if err := providerStatus(res.StatusCode); err != nil {
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
		if err := json.NewDecoder(io.LimitReader(res.Body, maxOutputBytes+1)).Decode(&completion); err != nil {
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
		if err := out.delta(lmbp.TaskDelta{Text: ch.Message.Content, ToolCalls: ch.Message.ToolCalls}); err != nil {
			return nil, err
		}
		if ch.FinishReason == "" {
			ch.FinishReason = "stop"
		}
		return &lmbp.TaskDone{FinishReason: ch.FinishReason, Usage: completion.Usage}, nil
	}
	var emitErr error
	done, fail := client.RelayOpenAIStream(ctx, res.Body, func(d lmbp.TaskDelta) error {
		emitErr = out.delta(d)
		return emitErr
	})
	if emitErr != nil {
		return nil, emitErr
	}
	if fail != nil {
		if fail.Code == lmbp.ErrCancelled {
			return nil, context.Canceled
		}
		return nil, knownFailure("UPSTREAM_ERROR: " + fail.Message)
	}
	return done, nil
}

func maxTokens(run lmbp.TaskRun, a AdapterConfig) int {
	if run.MaxTokens > 0 && (a.MaxOutputTokens == 0 || run.MaxTokens < a.MaxOutputTokens) {
		return run.MaxTokens
	}
	return a.MaxOutputTokens
}

// parseDataURI splits data:<mime>;base64,<data>.
func parseDataURI(u string) (string, string, bool) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", "", false
	}
	meta, data, ok := strings.Cut(rest, ",")
	mt, b64 := strings.CutSuffix(meta, ";base64")
	return mt, data, ok && b64
}
