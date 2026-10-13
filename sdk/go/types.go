package lsp2server

import (
	"github.com/tyro668/LocalSubProxy/internal/lsp2"
	"github.com/tyro668/LocalSubProxy/internal/lsp2/store"
)

// Protocol types re-exported so code outside this module can use them
// (internal/ packages are not importable from other modules).
type (
	ChatMessage  = lsp2.Message
	Content      = lsp2.Content
	ContentPart  = lsp2.ContentPart
	Tool         = lsp2.Tool
	ToolCall     = lsp2.ToolCall
	FunctionCall = lsp2.FunctionCall
	Usage        = lsp2.Usage
	JWK          = lsp2.JWK
	Device       = store.Device
)

// Text builds plain-text message content.
func Text(s string) Content { return lsp2.Text(s) }
