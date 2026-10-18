package lmbpserver_test

// Audit findings 7 and 8 (Go SDK): client gaps and in-process rate limiting.

import (
	"context"
	"errors"
	"strings"
	"testing"

	lmbpserver "github.com/tyro668/avora/sdk/go"
)

// Finding 8: Pairing.Approve/Deny count failures and lock out like the HTTP API.
func TestInProcessPairingRateLimited(t *testing.T) {
	r := newRig(t, lmbpserver.NewMemoryStore())
	ctx := context.Background()
	var limited int
	for i := 0; i < 12; i++ {
		_, err := r.srv.Pairing.Approve(ctx, "acme", "u1", "WRONG-000"+string(rune('A'+i)), "")
		if errors.Is(err, lmbpserver.ErrRateLimited) {
			limited++
		} else if !errors.Is(err, lmbpserver.ErrCodeInvalid) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if limited != 2 { // 10 failures allowed per minute, attempts 11 and 12 are refused
		t.Fatalf("in-process approvals bypass the failure limit: %d of 12 rate limited, want 2", limited)
	}
	if _, err := r.srv.Pairing.Deny(ctx, "acme", "u1", "WRONG-0000"); !errors.Is(err, lmbpserver.ErrRateLimited) {
		t.Fatalf("Deny bypasses the lockout: %v", err)
	}
}

// Finding 7: idempotency option, SSE event cursor, Last-Event-ID resume.
func TestClientIdempotencyCursorResume(t *testing.T) {
	r := newRig(t, lmbpserver.NewMemoryStore())
	r.pair(t, "u1")
	c, _ := r.srv.Client("acme", "u1")
	ctx := context.Background()
	req := lmbpserver.ChatRequest{Model: "m1", Messages: []lmbpserver.ChatMessage{{Role: "user", Content: lmbpserver.Text("hello")}}}
	st, err := c.ChatCompletionStream(ctx, req, lmbpserver.WithIdempotencyKey("k-1"))
	if err != nil {
		t.Fatal(err)
	}
	var ids, parts []string
	var text string
	for st.Next() {
		ids = append(ids, st.LastEventID())
		p := ""
		for _, ch := range st.Current().Choices {
			p += ch.Delta.Content
		}
		parts = append(parts, p)
		text += p
	}
	if st.Err() != nil || text != "echo:hello" || st.TaskID() == "" || len(ids) < 3 || ids[0] != "0" {
		t.Fatalf("stream err %v text %q task %q ids %v", st.Err(), text, st.TaskID(), ids)
	}
	// resume after the first content event: only the rest is replayed
	rs, err := c.ResumeChatCompletionStream(ctx, st.TaskID(), ids[1])
	if err != nil {
		t.Fatal(err)
	}
	var rest string
	for rs.Next() {
		for _, ch := range rs.Current().Choices {
			rest += ch.Delta.Content
		}
	}
	if want := strings.Join(parts[2:], ""); rs.Err() != nil || rest != want || rest == text {
		t.Fatalf("resume from %s: %q %v", ids[1], rest, rs.Err())
	}
	// the same idempotency key + body re-attaches (no second run)
	again, err := c.ChatCompletionStream(ctx, req, lmbpserver.WithIdempotencyKey("k-1"))
	if err != nil {
		t.Fatal(err)
	}
	var replay string
	for again.Next() {
		for _, ch := range again.Current().Choices {
			replay += ch.Delta.Content
		}
	}
	if replay != text || again.TaskID() != st.TaskID() {
		t.Fatalf("idempotent replay: %q task %q vs %q", replay, again.TaskID(), st.TaskID())
	}
	// a different body under the same key is refused, not silently re-run
	if _, err := c.ChatCompletion(ctx, req, lmbpserver.WithIdempotencyKey("k-1")); err == nil || !strings.Contains(err.Error(), "IDEMPOTENCY_KEY_REUSED") {
		t.Fatalf("key reuse with another body: %v", err)
	}
	// resume beyond the terminal event ends at once
	end, err := c.ResumeChatCompletionStream(ctx, st.TaskID(), "999")
	if err != nil {
		t.Fatal(err)
	}
	for end.Next() {
		t.Fatal("events after the terminal one")
	}
	if err := end.Err(); err != nil {
		t.Fatalf("resume past the terminal event must end with its terminator, got %v", err)
	}
}

// Finding 7: E2EE request/response from Go business code.
func TestClientE2EE(t *testing.T) {
	r := newRig(t, lmbpserver.NewMemoryStore())
	r.pair(t, "u1")
	c, _ := r.srv.Client("acme", "u1")
	ctx := context.Background()
	ds, _ := r.srv.Pairing.ListDevices(ctx, "acme", "u1")
	if len(ds) != 1 || ds[0].E2EEKey == "" {
		t.Fatalf("device e2ee key statement not exposed: %+v", ds)
	}
	sealed, err := lmbpserver.SealE2EERequest(ds[0], ds[0].JKT, lmbpserver.E2EERequest{Model: "m1",
		Messages: []lmbpserver.ChatMessage{{Role: "user", Content: lmbpserver.Text("secret")}}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.ChatCompletionStream(ctx, lmbpserver.ChatRequest{Model: ds[0].DeviceID + "/m1", E2EE: sealed.JWE})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	rd := sealed.Reader()
	for st.Next() {
		ch := st.Current()
		if ch.E2EE == nil {
			continue
		}
		p, err := rd.Next(*ch.E2EE)
		if err != nil {
			t.Fatal(err)
		}
		if ch.E2EE.Type == "task.delta" {
			d, _ := lmbpserver.DecodeE2EEDelta(p)
			text += d.Text
		}
	}
	if st.Err() != nil || rd.Finish() != nil || text != "echo:secret" {
		t.Fatalf("e2ee stream: %q %v %v", text, st.Err(), rd.Finish())
	}
}
