package connector

import "github.com/tyro668/avora/internal/backend/codex"

// Legacy entrypoints preserve the existing desktop/native login contract.
type CodexLogin = codex.CodexLogin

var CodexModels = codex.CodexModels
var BeginCodexLogin = codex.BeginCodexLogin
var LogoutCodex = codex.LogoutCodex
