package anthropic

import (
	"context"
	"net/http"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/httpapi"
	"github.com/tyro668/avora/internal/lmbp"
)

type Adapter struct {
	httpapi.Client
	modelClient *http.Client
}

func Definition() backend.Definition {
	return backend.Definition{
		Type: "anthropic_compatible", Name: "Anthropic 兼容", Label: "Anthropic 兼容协议", Short: "anthropic", Cloud: true, NeedsAPIKey: true, DefaultCapabilities: []string{"tools", "vision"}, Parameters: lmbp.ParamCaps,
		Limits:             backend.Limits{MaxOutput: true, MaxOutputVia: "max_tokens", DefaultMaxOutput: anthropicDefaultMaxTokens, ContextWindow: true},
		DefaultConcurrency: 4, Audio: false, Validate: httpapi.ValidateCloud,
		New: func(a backend.Config, d backend.Dependencies) backend.Backend {
			n := a.Concurrency
			if n <= 0 {
				n = 4
			}
			return &Adapter{Client: httpapi.Client{Config: a, ProbeClient: d.ProbeClient, Slots: n, Cloud: true, Authorize: func(r *http.Request, key string) {
				r.Header.Set("x-api-key", key)
				r.Header.Set("anthropic-version", "2023-06-01")
			}}, modelClient: d.ModelClient}
		},
	}
}
func (x *Adapter) Execute(ctx context.Context, run lmbp.TaskRun, out *backend.Emitter) (*lmbp.TaskDone, error) {
	return x.callAnthropic(ctx, run.Model, run, x.Config, out)
}
