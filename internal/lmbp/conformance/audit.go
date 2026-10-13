package conformance

// Z*: regression cases from the 2026-10 security/robustness audit. Each check
// pins one finding as black-box behaviour every implementation must share.

import (
	"context"
	"strings"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

func (r *runner) runAudit(ctx context.Context, dev *extDev, full func(string) string, mTools string) {
	r.auditIdempotency(ctx, dev, full(mTools))
}

// Z1: idempotency records are scoped to (tenant, end user, key) without any
// ambiguous concatenation: (user U, key "x|y") and (user "U|x", key "y") are
// different records, and a replay never reattaches another user's task.
func (r *runner) auditIdempotency(ctx context.Context, dev *extDev, model string) {
	body := map[string]any{"model": model, "stream": false, "messages": []any{map[string]any{"role": "user", "content": "idem"}}}
	type res struct {
		st   int
		body []byte
	}
	ch := make(chan res, 1)
	go func() {
		s, _, b := r.chatRaw(ctx, body, map[string]string{"Idempotency-Key": "x|y"})
		ch <- res{s, b}
	}()
	f, _, err := dev.await(lmbp.TypeTaskRun, nil)
	if err != nil {
		r.check("Z1", "idempotency key scoping", err)
		return
	}
	_ = dev.send(lmbp.TypeTaskDelta, f.TaskID, 1, lmbp.TaskDelta{Text: "owner-secret"})
	_ = dev.send(lmbp.TypeTaskDone, f.TaskID, 2, lmbp.TaskDone{FinishReason: "stop"})
	first := <-ch
	st, _, b := r.chatRaw(ctx, body, map[string]string{"Idempotency-Key": "y", "X-End-User": r.cfg.EndUser + "|x"})
	r.check("Z1", "(user U, Idempotency-Key \"x|y\") and (user \"U|x\", key \"y\") do not collide: the second user cannot replay the first user's task",
		want(first.st == 200 && strings.Contains(string(first.body), "owner-secret") && st != 200 && !strings.Contains(string(b), "owner-secret"),
			"first %d; second %d %s", first.st, st, b))
	st, _, b = r.chatRaw(ctx, body, map[string]string{"Idempotency-Key": "x|y"})
	r.check("Z1b", "the owner's replay with the same key and body still returns the original task",
		want(st == 200 && strings.Contains(string(b), "owner-secret"), "status %d %s", st, b))
}
