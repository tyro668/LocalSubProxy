package backend

import (
	"context"
	"fmt"
	"strings"
)

// ModelCatalog is optional: providers without capability metadata keep Models.
// An empty, known effort list means the model does not expose an adjustment.
type ModelCatalog interface {
	Catalog(context.Context) ([]ModelInfo, error)
}

type ModelInfo struct {
	ID                     string         `json:"id"`
	ReasoningKnown         bool           `json:"reasoning_known"`
	DefaultReasoningEffort string         `json:"default_reasoning_effort,omitempty"`
	ReasoningEfforts       []EffortOption `json:"reasoning_efforts"`
}

type EffortOption struct {
	Value string `json:"value"`
	// Model is set when an effort is selected by choosing a model variant.
	Model string `json:"model,omitempty"`
}

func ModelIDs(models []ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

// NormalizeEffort preserves the old wire meaning of off. Grok/Antigravity
// omitted it; Codex sent none. HTTP providers retain their previous mapping.
func NormalizeEffort(typ, effort string) string {
	if effort == "default" {
		return ""
	}
	if effort == "off" {
		if typ == "grok_build" || typ == "antigravity_cli" {
			return ""
		}
		if typ == "codex_app_server" {
			return "none"
		}
		return "off"
	}
	return effort
}

// ResolveEffort rejects unsupported overrides before inference. Default leaves
// the selected model unchanged and delegates the effort to the provider.
func ResolveEffort(models []ModelInfo, model, effort string) (string, error) {
	if effort == "" {
		return model, nil
	}
	for _, m := range models {
		if m.ID != model {
			continue
		}
		if !m.ReasoningKnown {
			break
		}
		values := []string{"default"}
		for _, option := range m.ReasoningEfforts {
			values = append(values, option.Value)
			if option.Value == effort {
				if option.Model != "" {
					return option.Model, nil
				}
				return model, nil
			}
		}
		return "", Failure(fmt.Sprintf("CAPABILITY_UNSUPPORTED: model %q does not support reasoning effort %q; supported: %s", model, effort, strings.Join(values, ", ")))
	}
	return "", Failure(fmt.Sprintf("CAPABILITY_UNSUPPORTED: reasoning capabilities for model %q are unknown; use default or refresh the model catalog", model))
}
