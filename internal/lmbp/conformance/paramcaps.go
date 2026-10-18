package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tyro668/avora/internal/lmbp"
)

// mParamCaps declares its request-parameter support ("params") and honours
// only temperature (spec §16.4).
const mParamCaps = "conf-params-temperature"

// Z8d: a model that declares its parameter support gets every undeclared
// parameter rejected with 400 CAPABILITY_UNSUPPORTED before any task.run is
// sent; declared ones are forwarded; on /v1/messages the mandatory
// max_tokens is dropped for a model without max_tokens support.
func (r *runner) auditParamCaps(ctx context.Context, dev *extDev, model string) {
	try := func(endpoint string, body map[string]any) (int, string, map[string]any) {
		cc, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
		defer cancel()
		type res struct {
			st int
			b  []byte
		}
		ch := make(chan res, 1)
		go func() {
			var st int
			var b []byte
			if endpoint == "messages" {
				st, _, b = r.messagesRaw(cc, body, nil)
			} else {
				st, _, b = r.chatRaw(cc, body, nil)
			}
			ch <- res{st, b}
		}()
		var run map[string]any
		for {
			select {
			case x := <-ch:
				if endpoint == "messages" {
					// Anthropic error shape carries no LMBP code: require
					// invalid_request_error naming the unsupported parameter.
					if anthErrType(x.b) == "invalid_request_error" && bytes.Contains(x.b, []byte("does not support the")) {
						return x.st, lmbp.ErrUnsupported, run
					}
					return x.st, anthErrType(x.b), run
				}
				return x.st, errCode(x.b), run
			case f := <-dev.frames:
				if f.Type != lmbp.TypeTaskRun {
					continue
				}
				if parts := strings.Split(f.JWS, "."); len(parts) == 3 {
					if p, err := lmbp.UnB64(parts[1]); err == nil {
						_ = json.Unmarshal(p, &run)
					}
				}
				_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 1, lmbp.TaskDone{FinishReason: "stop"})
			case <-cc.Done():
				return 0, "timeout", run
			}
		}
	}
	msgs := []any{map[string]any{"role": "user", "content": "param caps"}}
	var fails []string
	for _, c := range []struct {
		endpoint, param string
		set             map[string]any
		ok              bool
	}{
		{"chat", "temperature", map[string]any{"temperature": 0.5}, true},
		{"chat", "top_p", map[string]any{"top_p": 0.5}, false},
		{"chat", "stop", map[string]any{"stop": "END"}, false},
		{"chat", "max_tokens", map[string]any{"max_tokens": 10}, false},
		{"messages", "top_p", map[string]any{"max_tokens": 16, "top_p": 0.5}, false},
		{"messages", "stop_sequences", map[string]any{"max_tokens": 16, "stop_sequences": []any{"END"}}, false},
		{"messages", "max_tokens (mandatory, dropped)", map[string]any{"max_tokens": 16}, true},
	} {
		if c.endpoint == "messages" && !r.declares(lmbp.ProfileAnthropic) {
			continue
		}
		body := map[string]any{"model": model, "messages": msgs}
		for k, v := range c.set {
			body[k] = v
		}
		st, code, run := try(c.endpoint, body)
		name := c.endpoint + "/" + c.param
		switch {
		case c.ok && (st != 200 || run == nil):
			fails = append(fails, fmt.Sprintf("%s: %d %s, task.run seen %v (want accepted)", name, st, code, run != nil))
		case c.ok && c.endpoint == "messages" && run["max_tokens"] != nil:
			fails = append(fails, fmt.Sprintf("%s: task.run max_tokens = %v, want absent", name, run["max_tokens"]))
		case !c.ok && (st != 400 || code != lmbp.ErrUnsupported):
			fails = append(fails, fmt.Sprintf("%s: %d %s (want 400 %s)", name, st, code, lmbp.ErrUnsupported))
		case !c.ok && run != nil:
			fails = append(fails, name+": rejected but task.run reached the device")
		}
	}
	r.check("Z8d", "a model declaring its parameter support (\"params\" + flags) gets undeclared temperature/top_p/stop/max_tokens rejected with 400 CAPABILITY_UNSUPPORTED before task.run; on /v1/messages the mandatory max_tokens is dropped",
		want(len(fails) == 0, "%s", strings.Join(fails, "; ")))
}
