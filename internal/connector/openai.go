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
	"unicode/utf8"

	"localsubproxy/internal/protocol"
)

// callOpenAI accepts the Chat Completions SSE subset used by OpenAI-compatible providers.
// The configured key never enters the SaaS protocol, journal, health report or logs.
func (x *Connector) callOpenAI(ctx context.Context, c protocol.Command, a AdapterConfig) error {
	body, _ := json.Marshal(map[string]any{"model": c.Request.Target.ModelID, "messages": c.Request.Input.Messages, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if a.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	}
	res, err := x.modelClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case 200:
	case 401, 403:
		return knownFailure("AUTH_REQUIRED")
	case 429:
		return knownFailure("PROVIDER_RATE_LIMITED")
	default:
		return knownFailure(fmt.Sprintf("UPSTREAM_ERROR: provider HTTP %d", res.StatusCode))
	}
	emit := func(s string, total *int) error {
		if !utf8.ValidString(s) {
			return errors.New("invalid provider UTF-8")
		}
		*total += len(s)
		if *total > 10<<20 {
			return knownFailure("OUTPUT_LIMIT_EXCEEDED")
		}
		for len(s) > 0 {
			n := len(s)
			if n > 32<<10 {
				n = 32 << 10
				for n > 0 && !utf8.RuneStart(s[n]) {
					n--
				}
			}
			if err := x.emit(c, "output.delta", map[string]string{"channel": "answer", "text": s[:n]}); err != nil {
				return err
			}
			s = s[n:]
		}
		return nil
	}
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		var completion struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 10<<20+1)).Decode(&completion); err != nil {
			return err
		}
		if len(completion.Choices) == 0 {
			return errors.New("provider returned no choices")
		}
		total := 0
		return emit(completion.Choices[0].Message.Content, &total)
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	total := 0
	done := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			done = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return errors.New("invalid provider SSE JSON")
		}
		if chunk.Error != nil {
			return knownFailure("UPSTREAM_ERROR: provider stream error")
		}
		for _, choice := range chunk.Choices {
			if err := emit(choice.Delta.Content, &total); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !done {
		return errors.New("provider stream ended without done marker")
	}
	return nil
}
