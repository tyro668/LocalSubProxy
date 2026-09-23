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

	"localsubproxy/internal/protocol"
)

// callAnthropic translates the connector's text-only chat task to the
// Anthropic Messages API. Provider credentials remain in the local adapter.
func (x *Connector) callAnthropic(ctx context.Context, c protocol.Command, a AdapterConfig) error {
	maxTokens := a.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = 1024 // Messages requires max_tokens even when the UI leaves it automatic.
	}
	var system []string
	var messages []protocol.Message
	for _, message := range c.Request.Input.Messages {
		if message.Role == "system" {
			system = append(system, message.Content)
		} else {
			messages = append(messages, message)
		}
	}
	if len(messages) == 0 {
		return knownFailure("POLICY_DENIED: Anthropic Messages requires a user or assistant message")
	}
	payload := map[string]any{"model": c.Request.Target.ModelID, "messages": messages, "max_tokens": maxTokens, "stream": true}
	if len(system) > 0 {
		payload["system"] = strings.Join(system, "\n\n")
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, adapterEndpoint(a, "/messages"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
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
	total := 0
	if strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		var completion struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 10<<20+1)).Decode(&completion); err != nil {
			return err
		}
		foundText := false
		for _, block := range completion.Content {
			if block.Type == "text" {
				foundText = true
				if err := x.emitProviderText(c, block.Text, &total); err != nil {
					return err
				}
			}
		}
		if !foundText {
			return errors.New("provider returned no text content")
		}
		return nil
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var eventType string
	var data []string
	done := false
	consume := func() error {
		if len(data) == 0 {
			return nil
		}
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &event); err != nil {
			return errors.New("invalid Anthropic SSE JSON")
		}
		if event.Type == "" {
			event.Type = eventType
		}
		switch event.Type {
		case "content_block_delta":
			if event.Delta.Type == "text_delta" {
				return x.emitProviderText(c, event.Delta.Text, &total)
			}
		case "message_stop":
			done = true
		case "error":
			return knownFailure("UPSTREAM_ERROR: provider stream error")
		}
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := consume(); err != nil {
				return err
			}
			eventType, data = "", nil
			if done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !done {
		if err := consume(); err != nil {
			return err
		}
	}
	if !done {
		return errors.New("provider stream ended without message_stop")
	}
	return nil
}
