package gateway

// Request parameter validation (third audit #2). Every parameter the gateway
// understands is checked before anything is stored, queued or signed, and
// the signed task.run payload is then validated against
// spec/schemas/task.run.payload.json. Illegal values are client errors:
// 400 BAD_REQUEST with the offending top-level parameter in error.param
// (OpenAI shape) or error.lmbp_param (Anthropic shape). The same rules are
// implemented in the Node and Python SDKs and pinned by
// spec/testvectors/params.json and conformance Z8.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/spec/schemas"
)

const (
	MaxStopSequences = 4
	MaxStopLength    = 256
	MaxTools         = 128
)

var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ParamError is an illegal request parameter.
type ParamError struct{ Param, Msg string }

func (e *ParamError) Error() string { return e.Msg }

func perr(param, format string, a ...any) *ParamError {
	return &ParamError{Param: param, Msg: param + ": " + fmt.Sprintf(format, a...)}
}

// writeParamError writes 400 BAD_REQUEST in the endpoint's error shape.
func writeParamError(w http.ResponseWriter, e *ParamError) {
	if isAnthropic(w) {
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": e.Msg, "lmbp_code": lmbp.ErrBadRequest, "lmbp_param": e.Param}})
		return
	}
	writeJSON(w, 400, map[string]any{"error": map[string]any{"message": e.Msg, "type": "invalid_request_error", "code": lmbp.ErrBadRequest, "param": e.Param}})
}

// chatParams are the validated, normalized sampling parameters.
type chatParams struct {
	Temperature *float64
	TopP        *float64
	MaxTokens   int
	Stop        []string
}

func present(m map[string]any, k string) bool { v, ok := m[k]; return ok && v != nil }

func numberIn(m map[string]any, k string, lo, hi float64) (*float64, *ParamError) {
	if !present(m, k) {
		return nil, nil
	}
	f, ok := m[k].(float64)
	if !ok {
		return nil, perr(k, "must be a number")
	}
	if f < lo || f > hi || math.IsNaN(f) {
		return nil, perr(k, "must be between %g and %g, got %g", lo, hi, f)
	}
	return &f, nil
}

func positiveInt(m map[string]any, k string) (int, *ParamError) {
	if !present(m, k) {
		return 0, nil
	}
	f, ok := m[k].(float64)
	if !ok || f != math.Trunc(f) || f < 1 || f > math.MaxInt32 {
		return 0, perr(k, "must be a positive integer, got %s", jsonText(m[k]))
	}
	return int(f), nil
}

func boolean(m map[string]any, k string) *ParamError {
	if !present(m, k) {
		return nil
	}
	if _, ok := m[k].(bool); !ok {
		return perr(k, "must be a boolean")
	}
	return nil
}

func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }

// stopList validates stop (OpenAI: string or array) / stop_sequences
// (Anthropic: array) into 1..4 non-empty sequences.
func stopList(m map[string]any, k string, allowString bool) ([]string, *ParamError) {
	if !present(m, k) {
		return nil, nil
	}
	var list []any
	switch v := m[k].(type) {
	case string:
		if !allowString {
			return nil, perr(k, "must be an array of strings")
		}
		list = []any{v}
	case []any:
		list = v
	default:
		if allowString {
			return nil, perr(k, "must be a string or an array of strings")
		}
		return nil, perr(k, "must be an array of strings")
	}
	if len(list) < 1 || len(list) > MaxStopSequences {
		return nil, perr(k, "must have 1 to %d sequences, got %d", MaxStopSequences, len(list))
	}
	out := make([]string, len(list))
	for i, x := range list {
		s, ok := x.(string)
		if !ok || s == "" || lmbp.CharLen(s) > MaxStopLength {
			return nil, perr(k, "sequence %d must be a non-empty string of at most %d characters", i, MaxStopLength)
		}
		out[i] = s
	}
	return out, nil
}

// ValidateChatParams checks an OpenAI chat completions body (also what
// /v1/messages is translated into).
func ValidateChatParams(m map[string]any) (chatParams, *ParamError) {
	var p chatParams
	var e *ParamError
	if s, ok := m["model"].(string); !ok || s == "" {
		return p, perr("model", "field required (non-empty string)")
	}
	if e = boolean(m, "stream"); e != nil {
		return p, e
	}
	if p.Temperature, e = numberIn(m, "temperature", 0, 2); e != nil {
		return p, e
	}
	if p.TopP, e = numberIn(m, "top_p", 0, 1); e != nil {
		return p, e
	}
	mt, e := positiveInt(m, "max_tokens")
	if e != nil {
		return p, e
	}
	mct, e := positiveInt(m, "max_completion_tokens")
	if e != nil {
		return p, e
	}
	p.MaxTokens = mt
	if mt == 0 {
		p.MaxTokens = mct
	}
	if present(m, "n") {
		f, ok := m["n"].(float64)
		if !ok || f != math.Trunc(f) {
			return p, perr("n", "must be an integer")
		}
		if f != 1 {
			return p, perr("n", "only n=1 is supported, got %g", f)
		}
	}
	if p.Stop, e = stopList(m, "stop", true); e != nil {
		return p, e
	}
	_, e2ee := m["lmbp_e2ee"]
	if !e2ee {
		if e = validateOpenAIMessages(m); e != nil {
			return p, e
		}
	}
	names, e := validateOpenAITools(m)
	if e != nil {
		return p, e
	}
	if e = validateOpenAIToolChoice(m, names); e != nil {
		return p, e
	}
	return p, nil
}

func validateOpenAIMessages(m map[string]any) *ParamError {
	raw, ok := m["messages"]
	if !ok || raw == nil {
		return perr("messages", "field required")
	}
	list, ok := raw.([]any)
	if !ok {
		return perr("messages", "must be an array")
	}
	if len(list) == 0 || len(list) > lmbp.MaxMessages {
		return perr("messages", "1-%d messages required, got %d", lmbp.MaxMessages, len(list))
	}
	for i, x := range list {
		msg, ok := x.(map[string]any)
		if !ok {
			return perr("messages", "message %d must be an object", i)
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system", "user", "assistant", "tool", "developer":
		default:
			return perr("messages", "message %d: unsupported role %s", i, jsonText(msg["role"]))
		}
		switch msg["content"].(type) {
		case nil, string, []any:
		default:
			return perr("messages", "message %d: content must be a string, an array of content parts or null", i)
		}
		if role == "tool" {
			if id, _ := msg["tool_call_id"].(string); id == "" {
				return perr("messages", "message %d: tool messages need tool_call_id", i)
			}
			if msg["content"] == nil {
				return perr("messages", "message %d: tool messages need content", i)
			}
		}
		if tc, ok := msg["tool_calls"]; ok && tc != nil {
			calls, ok := tc.([]any)
			if !ok || role != "assistant" {
				return perr("messages", "message %d: tool_calls must be an array on an assistant message", i)
			}
			for j, c := range calls {
				cm, _ := c.(map[string]any)
				fn, _ := cm["function"].(map[string]any)
				if fn == nil {
					return perr("messages", "message %d: tool_calls[%d] needs a function", i, j)
				}
			}
		}
	}
	return nil
}

// validateOpenAITools returns the declared function names.
func validateOpenAITools(m map[string]any) (map[string]bool, *ParamError) {
	names := map[string]bool{}
	if !present(m, "tools") {
		return names, nil
	}
	list, ok := m["tools"].([]any)
	if !ok {
		return nil, perr("tools", "must be an array")
	}
	if len(list) > MaxTools {
		return nil, perr("tools", "at most %d tools, got %d", MaxTools, len(list))
	}
	for i, x := range list {
		t, _ := x.(map[string]any)
		if t == nil || t["type"] != "function" {
			return nil, perr("tools", "tool %d must have type function", i)
		}
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			return nil, perr("tools", "tool %d needs a function object", i)
		}
		name, _ := fn["name"].(string)
		if !toolNameRE.MatchString(name) {
			return nil, perr("tools", "tool %d: function name must match ^[A-Za-z0-9_-]{1,64}$, got %s", i, jsonText(fn["name"]))
		}
		if d, ok := fn["description"]; ok && d != nil {
			if _, ok := d.(string); !ok {
				return nil, perr("tools", "tool %d: description must be a string", i)
			}
		}
		if pa, ok := fn["parameters"]; ok && pa != nil {
			if _, ok := pa.(map[string]any); !ok {
				return nil, perr("tools", "tool %d: parameters must be a JSON Schema object", i)
			}
		}
		names[name] = true
	}
	return names, nil
}

func validateOpenAIToolChoice(m map[string]any, names map[string]bool) *ParamError {
	if !present(m, "tool_choice") {
		return nil
	}
	switch v := m["tool_choice"].(type) {
	case string:
		switch v {
		case "none":
			return nil
		case "auto", "required":
		default:
			return perr("tool_choice", "must be none, auto, required or a function object, got %q", v)
		}
	case map[string]any:
		fn, _ := v["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if v["type"] != "function" || name == "" {
			return perr("tool_choice", `object form must be {"type":"function","function":{"name":...}}`)
		}
		if !names[name] {
			return perr("tool_choice", "names function %q that is not in tools", name)
		}
	default:
		return perr("tool_choice", "must be a string or an object")
	}
	if len(names) == 0 {
		return perr("tool_choice", "requires tools")
	}
	return nil
}

// ValidateAnthropicParams checks a Messages body with Anthropic's names and
// ranges (temperature 0-1, max_tokens required, stop_sequences array).
func ValidateAnthropicParams(m map[string]any) *ParamError {
	if s, ok := m["model"].(string); !ok || s == "" {
		return perr("model", "field required (non-empty string)")
	}
	if !present(m, "max_tokens") {
		return perr("max_tokens", "field required (positive integer)")
	}
	if _, e := positiveInt(m, "max_tokens"); e != nil {
		return e
	}
	if e := boolean(m, "stream"); e != nil {
		return e
	}
	if _, e := numberIn(m, "temperature", 0, 1); e != nil {
		return e
	}
	if _, e := numberIn(m, "top_p", 0, 1); e != nil {
		return e
	}
	if _, e := stopList(m, "stop_sequences", false); e != nil {
		return e
	}
	raw, ok := m["messages"]
	if !ok || raw == nil {
		return perr("messages", "field required")
	}
	list, ok := raw.([]any)
	if !ok {
		return perr("messages", "must be an array")
	}
	if len(list) == 0 || len(list) > lmbp.MaxMessages {
		return perr("messages", "1-%d messages required, got %d", lmbp.MaxMessages, len(list))
	}
	for i, x := range list {
		msg, _ := x.(map[string]any)
		if msg == nil {
			return perr("messages", "message %d must be an object", i)
		}
		if r, _ := msg["role"].(string); r != "user" && r != "assistant" {
			return perr("messages", "message %d: role must be user or assistant", i)
		}
		switch msg["content"].(type) {
		case string, []any:
		default:
			return perr("messages", "message %d: content must be a string or an array of blocks", i)
		}
	}
	names := map[string]bool{}
	if present(m, "tools") {
		tl, ok := m["tools"].([]any)
		if !ok {
			return perr("tools", "must be an array")
		}
		if len(tl) > MaxTools {
			return perr("tools", "at most %d tools, got %d", MaxTools, len(tl))
		}
		for i, x := range tl {
			t, _ := x.(map[string]any)
			if t == nil {
				return perr("tools", "tool %d must be an object", i)
			}
			if ty, has := t["type"]; has && ty != nil && ty != "custom" {
				return perr("tools", "tool %d: server tool %v is not supported", i, ty)
			}
			name, _ := t["name"].(string)
			if !toolNameRE.MatchString(name) {
				return perr("tools", "tool %d: name must match ^[A-Za-z0-9_-]{1,64}$, got %s", i, jsonText(t["name"]))
			}
			if _, ok := t["input_schema"].(map[string]any); !ok {
				return perr("tools", "tool %d: input_schema (object) is required", i)
			}
			names[name] = true
		}
	}
	if present(m, "tool_choice") {
		tc, ok := m["tool_choice"].(map[string]any)
		if !ok {
			return perr("tool_choice", "must be an object")
		}
		switch tc["type"] {
		case "none":
			return nil
		case "auto", "any":
		case "tool":
			name, _ := tc["name"].(string)
			if name == "" {
				return perr("tool_choice", "type tool needs a name")
			}
			if !names[name] {
				return perr("tool_choice", "names tool %q that is not in tools", name)
			}
		default:
			return perr("tool_choice", "type must be auto, any, tool or none, got %s", jsonText(tc["type"]))
		}
		if len(names) == 0 {
			return perr("tool_choice", "requires tools")
		}
	}
	return nil
}

// ValidateTaskRun validates a task.run payload against the published schema
// (spec/schemas/task.run.payload.json); it runs before the payload is signed.
func ValidateTaskRun(run lmbp.TaskRun) *ParamError {
	sch, err := schemas.TaskRun()
	if err != nil {
		return &ParamError{Param: "", Msg: "task.run schema unavailable: " + err.Error()}
	}
	b, _ := json.Marshal(run)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return &ParamError{Msg: err.Error()}
	}
	err = sch.Validate(inst)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return &ParamError{Msg: "task.run payload: " + err.Error()}
	}
	leaf := ve
	for len(leaf.Causes) > 0 {
		leaf = leaf.Causes[0]
	}
	param := ""
	if len(leaf.InstanceLocation) > 0 {
		param = leaf.InstanceLocation[0]
	}
	msg := "/" + strings.Join(leaf.InstanceLocation, "/")
	if out := leaf.BasicOutput(); out != nil {
		for _, u := range out.Errors {
			if u.Error != nil {
				msg += ": " + u.Error.String()
				break
			}
		}
	}
	return &ParamError{Param: param, Msg: "request does not satisfy the task.run schema at " + msg}
}
