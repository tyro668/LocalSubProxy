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

// Effort is the wire value of a stored effort: "" for default, aliases
// applied.
func (d Definition) Effort(effort string) string {
	if effort == "default" {
		return ""
	}
	if v, ok := d.EffortAliases[effort]; ok {
		return v
	}
	return effort
}

// AllowsEffort reports whether a scheme of type d may store effort.
func (d Definition) AllowsEffort(effort string) bool {
	if effort == "" || effort == "default" {
		return true
	}
	for _, e := range d.Efforts {
		if e == effort {
			return true
		}
	}
	return false
}

// CheckEffort refuses an effort the live catalog does not offer for model.
// A catalog that says nothing about the model's efforts refuses too, unless
// d passes efforts through.
func CheckEffort(d Definition, models []ModelInfo, model, effort string) error {
	effort = d.Effort(effort)
	if effort == "" {
		return nil
	}
	if d.EffortPassthrough {
		known := false
		for _, m := range models {
			if m.ID == model {
				known = m.ReasoningKnown
			}
		}
		if !known {
			return nil
		}
	}
	_, err := ResolveEffort(models, model, effort)
	return err
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
