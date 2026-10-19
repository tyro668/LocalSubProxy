package backend

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tyro668/avora/internal/lmbp"
)

// Limits says which scheme token limits a backend type really enforces.
// A limit the backend cannot enforce must not be stored: Registry.Validate
// rejects it instead of silently ignoring it, and the desktop hides it.
type Limits struct {
	// MaxOutput: the scheme's MaxOutputTokens reaches the model runtime.
	MaxOutput bool `json:"max_output"`
	// MaxOutputVia names the wire field or setting that carries it.
	MaxOutputVia string `json:"max_output_via,omitempty"`
	// DefaultMaxOutput is what Avora sends when the scheme sets none (0:
	// nothing is sent and the provider's own default applies).
	DefaultMaxOutput int `json:"default_max_output,omitempty"`
	// ContextWindow: Avora checks each request's estimated input against the
	// window before execution (CheckContextBudget).
	ContextWindow bool `json:"context_window"`
	// ContextVia describes how the window is applied besides that check.
	ContextVia string `json:"context_via,omitempty"`
	// ContextDetect names the source the window is read from when the scheme
	// sets none (empty: no detection; the window is then unknown).
	ContextDetect string `json:"context_detect,omitempty"`
}

// ContextDetector reads a model's context window from the runtime. The
// returned source describes where the number came from.
type ContextDetector interface {
	DetectContextWindow(ctx context.Context, model string) (window int, source string, err error)
}

// Limit states shown to the user, per field.
const (
	LimitUnsupported   = "unsupported"    // the backend cannot enforce it; not storable
	LimitUser          = "user"           // set in the scheme
	LimitDetected      = "detected"       // read from the runtime (Source says where)
	LimitVendorDefault = "vendor_default" // supported, unset: the provider's own default applies
	LimitAvoraDefault  = "avora_default"  // supported, unset: Avora sends DefaultMaxOutput
	LimitUnknown       = "unknown"        // supported, unset, nothing detected: no check
)

type LimitState struct {
	State  string `json:"state"`
	Value  int    `json:"value,omitempty"`
	Source string `json:"source,omitempty"`
	// Estimate: the check compares an estimated token count, not a
	// tokenizer count (always true for the context window).
	Estimate bool `json:"estimate,omitempty"`
}

// ConfigLimits are the states a stored scheme implies without contacting the
// runtime: user-set values, vendor default / unknown, or unsupported.
func ConfigLimits(l Limits, a Config) map[string]LimitState {
	out := map[string]LimitState{}
	switch {
	case !l.MaxOutput:
		out["max_output_tokens"] = LimitState{State: LimitUnsupported}
	case a.MaxOutputTokens > 0:
		out["max_output_tokens"] = LimitState{State: LimitUser, Value: a.MaxOutputTokens, Source: l.MaxOutputVia}
	case l.DefaultMaxOutput > 0:
		out["max_output_tokens"] = LimitState{State: LimitAvoraDefault, Value: l.DefaultMaxOutput, Source: l.MaxOutputVia}
	default:
		out["max_output_tokens"] = LimitState{State: LimitVendorDefault}
	}
	switch {
	case !l.ContextWindow:
		out["context_window"] = LimitState{State: LimitUnsupported}
	case a.ContextWindow > 0:
		out["context_window"] = LimitState{State: LimitUser, Value: a.ContextWindow, Source: l.ContextVia, Estimate: true}
	default:
		out["context_window"] = LimitState{State: LimitUnknown, Source: l.ContextDetect, Estimate: true}
	}
	return out
}

func checkLimits(d Definition, a *Config) error {
	if a.MaxOutputTokens > 0 && !d.Limits.MaxOutput {
		return fmt.Errorf("%s cannot enforce a maximum output token limit; clear max_output_tokens", d.Name)
	}
	if a.ContextWindow > 0 && !d.Limits.ContextWindow {
		return fmt.Errorf("%s cannot enforce a context window; clear context_window", d.Name)
	}
	return nil
}

// EstimateInputTokens is a deliberately low estimate of a request's input
// tokens without a tokenizer: 6 ASCII bytes or 2 other characters per token
// (BPE tokenizers average about 4 bytes per English token and at least one
// token per 2 CJK characters), images and audio not counted. A request this
// estimate puts over the window is over it for practically any tokenizer; a
// request close to the window may still be refused by the provider, so the
// check is a guard against clear overruns, not an exact budget.
func EstimateInputTokens(run lmbp.TaskRun) int {
	ascii, other := 0, 0
	add := func(s string) {
		for _, r := range s {
			if r < utf8.RuneSelf {
				ascii++
			} else {
				other++
			}
		}
	}
	for _, m := range run.Messages {
		add(m.Role)
		add(m.Content.PlainText())
		for _, tc := range m.ToolCalls {
			add(tc.Function.Name)
			add(tc.Function.Arguments)
		}
	}
	for _, t := range run.Tools {
		add(t.Function.Name)
		add(t.Function.Description)
		add(string(t.Function.Parameters))
	}
	return (ascii+5)/6 + (other+1)/2
}

// OutputReserve is the output budget a request may use: the smaller of the
// request's max_tokens and the scheme's MaxOutputTokens (0: none set).
func OutputReserve(run lmbp.TaskRun, a Config) int {
	if run.MaxTokens > 0 && (a.MaxOutputTokens == 0 || run.MaxTokens < a.MaxOutputTokens) {
		return run.MaxTokens
	}
	return a.MaxOutputTokens
}

// CheckContextBudget refuses a request whose estimated input plus output
// reserve exceeds window, before anything is sent to the model.
func CheckContextBudget(window int, source string, run lmbp.TaskRun, a Config) *lmbp.ErrorBody {
	if window <= 0 {
		return nil
	}
	in, reserve := EstimateInputTokens(run), OutputReserve(run, a)
	if in+reserve <= window {
		return nil
	}
	msg := fmt.Sprintf("CONTEXT_BUDGET_EXCEEDED: estimated input of at least %d tokens", in)
	if reserve > 0 {
		msg += fmt.Sprintf(" plus %d reserved output tokens", reserve)
	}
	return &lmbp.ErrorBody{Code: lmbp.ErrBadRequest, Message: msg + fmt.Sprintf(" exceeds the %d-token context window (%s); shorten the conversation", window, source)}
}

var detected sync.Map // detectKey → detection

type detectKey struct{ typ, base, model string }
type detection struct {
	window int
	source string
	at     time.Time
}

// detectRetry is how long a failed detection is remembered.
const detectRetry = 10 * time.Minute

// DetectedWindow returns the cached detected window of model (0 when the
// backend cannot detect it or detection failed; a failure is retried after
// detectRetry).
func DetectedWindow(ctx context.Context, b Backend, a Config, model string) (int, string) {
	det, ok := b.(ContextDetector)
	if !ok {
		return 0, ""
	}
	k := detectKey{a.Type, a.BaseURL, model}
	if v, ok := detected.Load(k); ok {
		if d := v.(detection); d.window > 0 || time.Since(d.at) < detectRetry {
			return d.window, d.source
		}
	}
	w, src, err := det.DetectContextWindow(ctx, model)
	if err != nil || w <= 0 {
		detected.Store(k, detection{at: time.Now()})
		return 0, ""
	}
	detected.Store(k, detection{window: w, source: src, at: time.Now()})
	return w, src
}
