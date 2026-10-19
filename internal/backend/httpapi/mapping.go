package httpapi

import (
	"fmt"
	"strings"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/lmbp"
)

func Status(code int) error {
	switch code {
	case 200:
		return nil
	case 401, 403:
		return backend.Failure("AUTH_REQUIRED")
	case 429:
		return backend.Failure("PROVIDER_RATE_LIMITED")
	}
	return backend.Failure(fmt.Sprintf("UPSTREAM_ERROR: provider HTTP %d", code))
}

func MaxTokens(run lmbp.TaskRun, a backend.Config) int {
	if run.MaxTokens > 0 && (a.MaxOutputTokens == 0 || run.MaxTokens < a.MaxOutputTokens) {
		return run.MaxTokens
	}
	return a.MaxOutputTokens
}

// ParseDataURI splits data:<mime>;base64,<data>.
func ParseDataURI(u string) (string, string, bool) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", "", false
	}
	meta, data, ok := strings.Cut(rest, ",")
	mt, b64 := strings.CutSuffix(meta, ";base64")
	return mt, data, ok && b64
}
