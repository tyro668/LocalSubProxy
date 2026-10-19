// Package backend defines the local inference boundary. Implementations live
// in subpackages; this package must never import those implementations.
package backend

import (
	"context"
	"net/http"

	"github.com/tyro668/avora/internal/lmbp"
)

// Backend is configured for one scheme. Execute receives an upstream model ID,
// never a published scheme/model ID. It must stop on context cancellation or
// emitter failure and release all resources before returning. Instances may be
// used concurrently; per-request state must stay local to Execute.
// Backends take LMBP payload types directly; there is no second request model.
type Backend interface {
	Models(context.Context) ([]string, error)
	Probe(context.Context) Health
	Execute(context.Context, lmbp.TaskRun, *Emitter) (*lmbp.TaskDone, error)
}

type Dependencies struct {
	ProbeClient, ModelClient *http.Client
	// UserHomeDir resolves the official CLI configuration/credential location.
	// Injection keeps process integration tests independent of a developer's account.
	UserHomeDir func() (string, error)
}

// Definition is everything Avora knows about a backend type. The
// connector, the CLI and the desktop read these fields instead of switching
// on type names.
type Definition struct {
	Type    string   // stored scheme type, e.g. grok_build
	Name    string   // short display name
	Label   string   // display name in pickers
	Short   string   // CLI name, e.g. grok
	Aliases []string // further CLI names
	// Executable is the default CLI command; empty for HTTP backends.
	Executable string
	// LoginCommand signs the CLI in, run by the user in a terminal.
	LoginCommand string
	// Cloud: inference leaves the device (provider_cloud).
	Cloud       bool
	NeedsAPIKey bool
	// DefaultBaseURL is filled in when a scheme gives none.
	DefaultBaseURL                     string
	DefaultCapabilities                []string
	Parameters                         []string
	DefaultConcurrency, MaxConcurrency int
	InlineImagesOnly, Audio            bool
	// Efforts are the reasoning_effort values a scheme may store besides
	// "" and "default". EffortAliases rewrite one before use (Codex: off is
	// none). With EffortPassthrough an effort the live catalog cannot
	// confirm is sent unchanged instead of refused.
	Efforts           []string
	EffortAliases     map[string]string
	EffortPassthrough bool
	Validate          func(*Config, bool) error
	New               func(Config, Dependencies) Backend
}

// HTTPEfforts are the efforts the OpenAI-compatible backend maps onto
// reasoning_effort (off is sent only where the provider defines it).
var HTTPEfforts = []string{"off", "low", "medium", "high"}

// Names are the CLI names of d, Short first.
func (d Definition) Names() []string {
	names := []string{}
	if d.Short != "" {
		names = append(names, d.Short)
	}
	return append(names, d.Aliases...)
}
