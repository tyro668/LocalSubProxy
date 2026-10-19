package ollama

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
		Type: "ollama_http", Name: "Ollama", Label: "Ollama 协议（本地模型）", Short: "ollama", DefaultBaseURL: "http://127.0.0.1:11434", DefaultCapabilities: []string{"tools"}, Parameters: lmbp.ParamCaps,
		DefaultConcurrency: 4, InlineImagesOnly: true, Audio: false, Validate: httpapi.ValidateLocal,
		New: func(a backend.Config, d backend.Dependencies) backend.Backend {
			n := a.Concurrency
			if n <= 0 {
				n = 4
			}
			return &Adapter{Client: httpapi.Client{Config: a, ProbeClient: d.ProbeClient, Slots: n}, modelClient: d.ModelClient}
		},
	}
}
func (x *Adapter) Execute(ctx context.Context, run lmbp.TaskRun, out *backend.Emitter) (*lmbp.TaskDone, error) {
	return x.callOllama(ctx, run.Model, run, x.Config, out)
}
