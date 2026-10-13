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

// The complete public Store contract (audit finding 10): every type that
// appears in a Store / SecretStore method signature, plus its sentinel
// errors, so a database adapter can live in another module. See
// examples/customstore for an implementation using only these names.
type (
	StoreTenant = store.Tenant // a tenant record (Tenant is the Options entry)
	DeviceAuth  = store.DeviceAuth
	Idempotency = store.Idempotency
	Task        = store.Task
	Event       = store.Event
)

var (
	ErrNotFound = store.ErrNotFound // Get*/lookup of a missing record
	ErrExists   = store.ErrExists   // PutIdempotency: (tenant, owner, key) already taken
)

// Text builds plain-text message content.
func Text(s string) Content { return lmbp.Text(s) }
