package openai

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
		Type: "openai_compatible", Name: "OpenAI 兼容", Label: "OpenAI 兼容协议", Short: "openai", Cloud: true, NeedsAPIKey: true, Efforts: backend.HTTPEfforts, DefaultCapabilities: []string{"tools", "vision"}, Parameters: lmbp.ParamCaps,
		DefaultConcurrency: 4, Audio: true, Validate: httpapi.ValidateCloud,
		New: func(a backend.Config, d backend.Dependencies) backend.Backend {
			n := a.Concurrency
			if n <= 0 {
				n = 4
			}
			return &Adapter{Client: httpapi.Client{Config: a, ProbeClient: d.ProbeClient, Slots: n, Cloud: true, Authorize: func(r *http.Request, key string) { r.Header.Set("Authorization", "Bearer "+key) }}, modelClient: d.ModelClient}
		},
	}
}
func (x *Adapter) Execute(ctx context.Context, run lmbp.TaskRun, out *backend.Emitter) (*lmbp.TaskDone, error) {
	return x.callOpenAI(ctx, run.Model, run, x.Config, out)
}
