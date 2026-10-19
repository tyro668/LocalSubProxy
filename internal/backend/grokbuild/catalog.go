package grokbuild

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/cliruntime"
	"github.com/tyro668/avora/internal/backend/jsonrpc"
)

// Catalog uses ACP's per-model metadata. No prompt or config mutation is sent.
func (a *Adapter) Catalog(ctx context.Context) ([]backend.ModelInfo, error) {
	// The text catalog checks authentication; ACP can expose fallback models
	// even when the CLI is signed out.
	ids, err := a.Models(ctx)
	if err != nil {
		return nil, err
	}
	o, cleanup, err := a.prepare()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// The read-only sandbox is a second fence: discovery never prompts, but
	// the ACP agent must not be able to write even if it tried.
	o.Args = append(o.Args, "--sandbox", "read-only", "agent", "stdio")
	initial, err := jsonrpc.Request("2.0", 1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "avora", "version": "0.1.0"}})
	if err != nil {
		return nil, err
	}
	complete := errors.New("catalog complete")
	var models []backend.ModelInfo
	wantID := 1
	err = cliruntime.Exchange(ctx, o, initial, func(line []byte, in io.WriteCloser) error {
		msg, err := jsonrpc.Parse(line)
		if err != nil {
			return err
		}
		if msg.IsRequest() {
			return backend.Failure("POLICY_DENIED: unexpected Grok request during model discovery")
		}
		if msg.IsNotification() {
			return nil
		}
		if id, ok := msg.IntID(); !ok || id != wantID {
			return backend.Failure("PROTOCOL_MISMATCH: unexpected Grok catalog response")
		}
		if msg.Error != nil {
			return backend.CLIFailure("Grok model discovery", msg.Error.Message)
		}
		if wantID == 1 {
			wantID = 2
			next, err := jsonrpc.Request("2.0", 2, "session/new", map[string]any{"cwd": o.Dir, "mcpServers": []any{}})
			if err != nil {
				return err
			}
			_, err = in.Write(next)
			return err
		}
		models, err = parseCatalog(msg.Result)
		if err != nil {
			return err
		}
		return complete
	})
	if !errors.Is(err, complete) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	// Keep authenticated model IDs, including models whose older metadata is
	// absent. Unknown capabilities permit default only, never invented levels.
	byID := map[string]backend.ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	result := make([]backend.ModelInfo, 0, len(ids))
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			m = backend.ModelInfo{ID: id, ReasoningEfforts: []backend.EffortOption{}}
		}
		result = append(result, m)
	}
	return result, nil
}

func parseCatalog(raw []byte) ([]backend.ModelInfo, error) {
	var session struct {
		Models struct {
			Available []struct {
				ID   string `json:"modelId"`
				Meta struct {
					Supports *bool  `json:"supportsReasoningEffort"`
					Effort   string `json:"reasoningEffort"`
					Efforts  []struct {
						ID      string `json:"id"`
						Value   string `json:"value"`
						Default bool   `json:"default"`
					} `json:"reasoningEfforts"`
				} `json:"_meta"`
			} `json:"availableModels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return nil, err
	}
	if len(session.Models.Available) == 0 {
		return nil, backend.Failure("PROTOCOL_MISMATCH: Grok ACP returned no model metadata")
	}
	var models []backend.ModelInfo
	for _, m := range session.Models.Available {
		info := backend.ModelInfo{ID: m.ID, ReasoningKnown: m.Meta.Supports != nil, ReasoningEfforts: []backend.EffortOption{}}
		if m.Meta.Supports != nil && *m.Meta.Supports {
			info.DefaultReasoningEffort = m.Meta.Effort
			for _, e := range m.Meta.Efforts {
				v := e.Value
				if v == "" {
					v = e.ID
				}
				if v == "" {
					continue
				}
				info.ReasoningEfforts = append(info.ReasoningEfforts, backend.EffortOption{Value: v})
				if e.Default {
					info.DefaultReasoningEffort = v
				}
			}
		}
		models = append(models, info)
	}
	return models, nil
}
