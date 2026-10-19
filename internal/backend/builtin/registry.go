// Package builtin is the composition root for the shipped backends.
package builtin

import (
	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/anthropic"
	"github.com/tyro668/avora/internal/backend/antigravity"
	"github.com/tyro668/avora/internal/backend/codex"
	"github.com/tyro668/avora/internal/backend/grokbuild"
	"github.com/tyro668/avora/internal/backend/ollama"
	"github.com/tyro668/avora/internal/backend/openai"
)

func NewRegistry() *backend.Registry {
	return backend.NewRegistry(codex.Definition(), openai.Definition(), anthropic.Definition(), ollama.Definition(), grokbuild.Definition(), antigravity.Definition())
}
