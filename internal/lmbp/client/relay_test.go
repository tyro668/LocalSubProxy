package client

import (
	"context"
	"strings"
	"testing"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Review7 #12: completion tracking and parse-error policy of the relay.
func TestRelayOpenAIStreamCompletion(t *testing.T) {
	chunk := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"
	fin := `data: {"choices":[{"delta":{},"finish_reason":"length"}]}` + "\n\n"
	cases := []struct {
		name, in string
		ok       bool
		finish   string
		msg      string
	}{
		{"done marker", chunk + "data: [DONE]\n\n", true, "stop", ""},
		{"finish reason then EOF", chunk + fin, true, "length", ""},
		{"no-space data prefix", "data:" + strings.TrimPrefix(chunk, "data: ") + "data:[DONE]\n", true, "stop", ""},
		{"comments ignored", ": ping\n\nevent: x\n" + chunk + "data: [DONE]\n", true, "stop", ""},
		{"EOF before done", chunk, false, "", "ended before completion"},
		{"empty stream", "", false, "", "ended before completion"},
		{"malformed chunk", chunk + "data: {not json\n\ndata: [DONE]\n", false, "", "malformed"},
		{"in-stream error", chunk + `data: {"error":{"message":"overloaded"}}` + "\n\n", false, "", "overloaded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			done, fail := RelayOpenAIStream(context.Background(), strings.NewReader(c.in), func(lmbp.TaskDelta) error { return nil })
			if c.ok {
				if fail != nil || done == nil || done.FinishReason != c.finish {
					t.Fatalf("want done(%s), got %+v %+v", c.finish, done, fail)
				}
				return
			}
			if done != nil || fail == nil || fail.Code != lmbp.ErrUpstream || !strings.Contains(fail.Message, c.msg) {
				t.Fatalf("want UPSTREAM_ERROR(%s), got %+v %+v", c.msg, done, fail)
			}
		})
	}
}
