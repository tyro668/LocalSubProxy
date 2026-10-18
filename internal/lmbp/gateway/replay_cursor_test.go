package gateway

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/tyro668/avora/internal/lmbp"
)

// audit3 P2-3: replaying a finished non-streaming idempotent request with a
// Last-Event-ID at or past the terminal seq must return the complete
// aggregated response (the cursor only applies to SSE), never dereference a
// missing terminal event. Was: nil pointer panic on done.FinishReason.
func TestNonStreamingReplayIgnoresLastEventID(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	first := idemChat(t, r, "echo", "one two", "k-replay")
	if first.code != 200 {
		t.Fatalf("first %d %s", first.code, first.body)
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, cursor := range []string{"3", "999"} {
			body := map[string]any{"model": "echo", "messages": user("one two")}
			if path == "/v1/messages" {
				body["max_tokens"] = 16
			}
			key := "k-replay"
			if path == "/v1/messages" {
				key = "k-replay-anthropic"
				if cursor == "3" { // run it once first
					resp := doReq(t, r.req("POST", path, body, map[string]string{"Idempotency-Key": key}))
					_, _ = io.ReadAll(resp.Body)
					resp.Body.Close()
				}
			}
			resp, err := httpc.Do(r.req("POST", path, body, map[string]string{"Idempotency-Key": key, "Last-Event-ID": cursor}))
			if err != nil {
				t.Fatalf("%s Last-Event-ID %s: %v (handler panicked?)", path, cursor, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var m map[string]any
			if resp.StatusCode != 200 || json.Unmarshal(b, &m) != nil {
				t.Fatalf("%s Last-Event-ID %s: %d %s", path, cursor, resp.StatusCode, b)
			}
			if path == "/v1/chat/completions" && string(b) != first.body {
				t.Fatalf("replay with Last-Event-ID %s differs:\n first  %s\n replay %s", cursor, first.body, b)
			}
		}
	}
}
