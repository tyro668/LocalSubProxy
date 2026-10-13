package lmbpserver

import (
	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// Protocol types re-exported so code outside this module can use them
// (internal/ packages are not importable from other modules).
type (
	ChatMessage  = lmbp.Message
	Content      = lmbp.Content
	ContentPart  = lmbp.ContentPart
	Tool         = lmbp.Tool
	ToolCall     = lmbp.ToolCall
	FunctionCall = lmbp.FunctionCall
	Usage        = lmbp.Usage
	JWK          = lmbp.JWK
	Device       = store.Device
)

// Text builds plain-text message content.
func Text(s string) Content { return lmbp.Text(s) }
