package backend

import (
	"strings"

	"github.com/tyro668/avora/internal/lmbp"
)

func (r *Registry) CheckRequest(a Config, run lmbp.TaskRun) *lmbp.ErrorBody {
	caps := r.Capabilities(a)
	d, err := r.Lookup(a.Type)
	if err != nil {
		return &lmbp.ErrorBody{Code: lmbp.ErrUnsupported, Message: err.Error()}
	}
	unsupported := func(msg string) *lmbp.ErrorBody {
		return &lmbp.ErrorBody{Code: lmbp.ErrUnsupported, Message: msg}
	}
	for flag, set := range map[string]bool{lmbp.ModelCapTemperature: run.Temperature != nil, lmbp.ModelCapTopP: run.TopP != nil,
		lmbp.ModelCapStop: len(run.Stop) > 0, lmbp.ModelCapMaxTokens: run.MaxTokens > 0} {
		if set && !Has(caps, flag) {
			return unsupported("model does not support the " + flag + " parameter")
		}
	}
	if (len(run.Tools) > 0 || len(run.ToolChoice) > 0) && !Has(caps, lmbp.ModelCapTools) {
		return unsupported("model does not support tools")
	}
	for _, m := range run.Messages {
		if (m.Role == "tool" || len(m.ToolCalls) > 0) && !Has(caps, lmbp.ModelCapTools) {
			return unsupported("model does not support tool messages")
		}
		for _, p := range m.Content.Parts {
			switch {
			case p.ImageURL != nil && !Has(caps, lmbp.ModelCapVision):
				return unsupported("model does not accept images")
			case p.ImageURL != nil && d.InlineImagesOnly && !strings.HasPrefix(p.ImageURL.URL, "data:"):
				return unsupported("Ollama accepts inline (data:) images only")
			case p.InputAudio != nil && (!Has(caps, lmbp.ModelCapAudio) || !d.Audio):
				return unsupported("model does not accept audio")
			}
		}
	}
	if d.Limits.ContextWindow && a.ContextWindow > 0 {
		return CheckContextBudget(a.ContextWindow, "scheme setting", run, a)
	}
	return nil
}

func Has(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
