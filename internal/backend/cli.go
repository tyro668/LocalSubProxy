package backend

import (
	"context"
	"strings"

	"github.com/tyro668/avora/internal/lmbp"
)

// TextInput renders a conversation as role-tagged text for CLI backends.
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

// LoginStatus is whether a CLI's own sign-in is usable. Avora never sees
// the credentials; Command is what the user runs in a terminal to sign in.
type LoginStatus struct {
	State   string `json:"state"` // ready, required or unknown
	Command string `json:"command,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// LoginChecker is implemented by backends whose CLI owns the sign-in.
type LoginChecker interface {
	LoginStatus(context.Context) LoginStatus
}

// LoginFromModels derives the sign-in state from a model listing, which
// every supported CLI refuses with AUTH_REQUIRED when signed out.
func LoginFromModels(ctx context.Context, d Definition, models func(context.Context) ([]string, error)) LoginStatus {
	s := LoginStatus{State: "ready", Command: d.LoginCommand}
	if _, err := models(ctx); err != nil {
		s.State, s.Detail = "unknown", err.Error()
		if strings.HasPrefix(err.Error(), "AUTH_REQUIRED") {
			s.State = "required"
		}
	}
	return s
}
