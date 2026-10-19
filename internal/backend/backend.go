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
// LMBP payload types are retained deliberately to preserve existing mappings.
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
type Definition struct {
	Type, Name, Executable             string
	DefaultCapabilities                []string
	Parameters                         []string
	DefaultConcurrency, MaxConcurrency int
	InlineImagesOnly, Audio            bool
	Validate                           func(*Config, bool) error
	New                                func(Config, Dependencies) Backend
}
