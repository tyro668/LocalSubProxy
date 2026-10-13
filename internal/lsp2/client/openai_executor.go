package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"localsubproxy/internal/lsp2"
)

// OpenAIExecutor runs tasks against a local OpenAI-compatible endpoint
// (LM Studio, Ollama /v1, llama.cpp server). Used by the mock and CLI.
type OpenAIExecutor struct {
	BaseURL   string
	ModelList []string
}

func (o *OpenAIExecutor) Models() []string { return o.ModelList }

func (o *OpenAIExecutor) Execute(ctx context.Context, run lsp2.TaskRun, emit func(string) error) (*lsp2.TaskDone, *lsp2.ErrorBody) {
	body := map[string]any{"model": run.Model, "messages": run.Messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
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
			return nil, &lsp2.ErrorBody{Code: lsp2.ErrCancelled, Message: "cancelled"}
		}
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrUpstream, Message: "local model unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrUpstream, Message: fmt.Sprintf("local model returned HTTP %d", resp.StatusCode)}
	}
	done := &lsp2.TaskDone{FinishReason: "stop"}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
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
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *lsp2.Usage `json:"usage"`
		}
		if json.Unmarshal([]byte(p), &ch) != nil {
			continue
		}
		if ch.Usage != nil {
			done.Usage = ch.Usage
		}
		for _, x := range ch.Choices {
			if x.Delta.Content != "" {
				if emit(x.Delta.Content) != nil {
					return nil, &lsp2.ErrorBody{Code: lsp2.ErrDeviceOffline, Message: "session lost"}
				}
			}
			if x.FinishReason != nil {
				done.FinishReason = *x.FinishReason
			}
		}
	}
	if ctx.Err() != nil {
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrCancelled, Message: "cancelled"}
	}
	if sc.Err() != nil {
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrUpstream, Message: "stream interrupted"}
	}
	return done, nil
}
