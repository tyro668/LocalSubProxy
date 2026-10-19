package codex

import (
	"context"
	"errors"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/lmbp"
)

type Adapter struct{ config backend.Config }

func Definition() backend.Definition {
	return backend.Definition{
		Type: "codex_app_server", Name: "Codex", Label: "OpenAI Codex（订阅）", Short: "codex", Executable: "codex", LoginCommand: "codex login", Cloud: true,
		Efforts: []string{"off", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, EffortAliases: map[string]string{"off": "none"}, EffortPassthrough: true, DefaultConcurrency: 1, MaxConcurrency: 1,
		Validate: func(a *backend.Config, required bool) error {
			if len(a.Capabilities) > 0 {
				return errors.New("the Codex adapter is text-only")
			}
			if a.CodexBin == "" {
				a.CodexBin = "codex"
			}
			path, err := resolveCodex(a.CodexBin)
			if err != nil {
				if required {
					return err
				}
				return nil
			}
			a.CodexBin = path
			return nil
		},
		New: func(a backend.Config, _ backend.Dependencies) backend.Backend { return &Adapter{config: a} },
	}
}
func (a *Adapter) Models(ctx context.Context) ([]string, error) {
	return CodexModels(ctx, a.config.CodexBin)
}
func (a *Adapter) Catalog(ctx context.Context) ([]backend.ModelInfo, error) {
	return CodexCatalog(ctx, a.config.CodexBin)
}
func (a *Adapter) Probe(ctx context.Context) backend.Health { return Probe(ctx, a.config) }
func (a *Adapter) Execute(ctx context.Context, r lmbp.TaskRun, out *backend.Emitter) (*lmbp.TaskDone, error) {
	usage, err := runCodexWithEffort(ctx, r.Model, r.Messages, a.config.CodexBin, a.config.ReasoningEffort, out.Text)
	return &lmbp.TaskDone{FinishReason: "stop", Usage: usage}, err
}

func (a *Adapter) LoginStatus(ctx context.Context) backend.LoginStatus {
	return backend.LoginFromModels(ctx, Definition(), a.Models)
}
