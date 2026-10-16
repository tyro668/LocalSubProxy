package connector

import (
	"context"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/audit"
	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Review5 P2-b: Shutdown must wait for a finished task's audit write, so
// stop (which closes the audit DB right after) never loses the last rows.
func TestShutdownWaitsForAuditWrite(t *testing.T) {
	a := testLink("ba", "https://gw.a", "acme", &Policy{Allow: []string{"*"}})
	x, al, _ := newPool(t, 4, a)
	entered, release := make(chan struct{}), make(chan struct{})
	auditHook = func() { close(entered); <-release }
	defer func() { auditHook = nil }()
	go x.serve(context.Background(), a, task("task_1", "lm/m1"), func(lmbp.TaskDelta) error { return nil })
	<-entered // inference returned; the audit write is pending
	down := make(chan struct{})
	go func() { x.Shutdown(); close(down) }()
	select {
	case <-down:
		t.Fatal("Shutdown returned before the audit row was written")
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	<-down
	recs, err := al.List(context.Background(), audit.Filter{})
	if err != nil || len(recs) != 1 {
		t.Fatalf("audit rows %d %v", len(recs), err)
	}
}

// Review6 P2: a request rejected before admission (link closing) still
// writes an audit row; Shutdown waits for it, and none starts after.
func TestShutdownWaitsForEarlyRejectAudit(t *testing.T) {
	a := testLink("ba", "https://gw.a", "acme", &Policy{Allow: []string{"*"}})
	x, al, _ := newPool(t, 4, a)
	x.Fence("ba")
	entered, release := make(chan struct{}), make(chan struct{})
	auditHook = func() { close(entered); <-release }
	defer func() { auditHook = nil }()
	go x.serve(context.Background(), a, task("task_1", "lm/m1"), func(lmbp.TaskDelta) error { return nil })
	<-entered
	down := make(chan struct{})
	go func() { x.Shutdown(); close(down) }()
	select {
	case <-down:
		t.Fatal("Shutdown returned before the early reject's audit row was written")
	case <-time.After(500 * time.Millisecond):
	}
	close(release)
	<-down
	auditHook = nil
	if _, fail := x.serve(context.Background(), a, task("task_2", "lm/m1"), func(lmbp.TaskDelta) error { return nil }); fail == nil {
		t.Fatal("request accepted after Shutdown")
	}
	recs, err := al.List(context.Background(), audit.Filter{})
	if err != nil || len(recs) != 1 || recs[0].TaskID != "task_1" {
		t.Fatalf("audit rows %+v %v", recs, err)
	}
}
