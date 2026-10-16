package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Review7 #13: device output over max_frame_bytes is split at the final
// encoding layer (text and tool arguments arrive intact), and an
// unsplittable piece fails the task instead of enqueuing a giant frame.
func TestOversizedDeltasAreSplitOrFail(t *testing.T) {
	r := newRig(t, lmbp.Flow{TaskWindow: lmbp.DefaultTaskWindow, ConnWindow: lmbp.DefaultConnWindow}, 1, time.Millisecond)
	ask := func(p string) (int, map[string]any) {
		return r.api(t, "POST", "/v1/chat/completions", map[string]any{"model": r.model("huge"), "messages": []map[string]string{{"role": "user", "content": p}}}, nil)
	}
	code, m := ask("text")
	msg := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if code != 200 || msg["content"] != hugeText {
		t.Fatalf("text: %d, len %d want %d", code, len(toStr(msg["content"])), len(hugeText))
	}
	code, m = ask("args")
	msg = m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls, _ := msg["tool_calls"].([]any)
	if code != 200 || msg["content"] != "calling" || len(calls) != 2 {
		t.Fatalf("args: %d content=%v calls=%d", code, msg["content"], len(calls))
	}
	for i, tag := range []string{"a", "b"} {
		fn := calls[i].(map[string]any)["function"].(map[string]any)
		if fn["arguments"] != hugeArgs(tag) || calls[i].(map[string]any)["id"] != tag {
			t.Fatalf("call %d args corrupted (len %d)", i, len(toStr(fn["arguments"])))
		}
	}
	code, m = ask("noidx")
	if code == 200 || !strings.Contains(toString(m), "max_frame_bytes") {
		t.Fatalf("unsplittable: %d %v", code, toString(m))
	}
}

func toStr(v any) string { s, _ := v.(string); return s }
