package backend

import (
	"context"
	"strings"

	"github.com/tyro668/avora/internal/lmbp"
)

// TextInput preserves the existing Codex role-tagged conversation mapping.
func TextInput(messages []lmbp.Message) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString("[" + m.Role + "]\n")
		b.WriteString(m.Content.PlainText())
		b.WriteString("\n\n")
	}
	return b.String()
}

// ProbeCLI is shared health bookkeeping, not a vendor protocol implementation.
func ProbeCLI(ctx context.Context, a Config, models func(context.Context) ([]string, error)) Health {
	h := Health{AdapterID: a.ID, AdapterType: a.Type, Health: "unavailable", AuthState: "unknown", InferenceLocation: "provider_cloud", FreeSlots: 1}
	if !a.Enabled {
		h.Health = "disabled"
		h.FreeSlots = 0
		return h
	}
	ids, err := models(ctx)
	if err != nil {
		h.Error = err.Error()
		if strings.HasPrefix(err.Error(), "AUTH_REQUIRED") {
			h.AuthState = "required"
		}
		return h
	}
	h.AuthState = "ready"
	for _, m := range a.Models {
		if Has(ids, m) {
			h.Models = append(h.Models, m)
		}
	}
	if len(h.Models) > 0 {
		h.Health = "ready"
	} else {
		h.Error = "selected model is unavailable"
	}
	return h
}
