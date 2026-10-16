package lmbp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Review9 P2-7 (desktop client resume): a task's terminal frame waits for
// flow credit; on resume the gateway's list no longer has the task, so the
// client Forgets it. Forget used to release the credit and wake the waiting
// send, which then re-created the task's outbox state. Before the new
// sender is attached that state is paused (written=-1) and it is not in the
// retransmit list, so its bytes stay charged to the connection window for
// good. A forgotten task must stay forgotten: the waiting send fails, no
// state is re-created and all of its credit is released.
func TestForgetDoesNotResurrectWaitingSend(t *testing.T) {
	o := NewOutbox()
	f := &Flow{TaskWindow: MaxFrameBytes, ConnWindow: MaxFrameBytes}
	o.SetFlow(f)
	var wrote int
	gen := o.AttachOwned(func([]byte) error { wrote++; return nil })
	_ = o.Retransmit("A", 0)
	big := strings.Repeat("x", MaxFrameBytes/2+1024)
	ctx := context.Background()
	if _, err := o.Send(ctx, Envelope{Type: TypeTaskDelta, TaskID: "A", Data: []byte(`{"text":"` + big + `"}`)}); err != nil {
		t.Fatal(err)
	}
	// The terminal frame does not fit: it waits for credit.
	res := make(chan error, 1)
	go func() {
		_, err := o.Send(ctx, Envelope{Type: TypeTaskDone, TaskID: "A", Data: []byte(`{"finish_reason":"stop","pad":"` + big + `"}`)})
		res <- err
	}()
	for o.Waiting() == 0 {
		time.Sleep(time.Millisecond)
	}
	// The connection drops; on the resume welcome the task is unknown to the gateway.
	o.Detach(gen)
	o.Forget("A")
	select {
	case err := <-res:
		if err == nil {
			t.Errorf("the waiting terminal send was admitted after Forget")
		}
	case <-time.After(time.Second):
		t.Fatal("the waiting send neither failed nor returned")
	}
	// The new connection's sender is attached; retransmit covers only the
	// gateway's resume list (empty here).
	o.AttachOwned(func([]byte) error { wrote++; return nil })
	if frames, bytes := o.Usage(); frames != 0 || bytes != 0 {
		t.Fatalf("forgotten task still charges the connection window: %d frame(s), %d bytes", frames, bytes)
	}
	if ts := o.Tasks(); len(ts) != 0 {
		t.Fatalf("forgotten task state re-created: %v", ts)
	}
	if _, err := o.Send(ctx, Envelope{Type: TypeTaskDelta, TaskID: "A", Data: []byte(`{"text":"late"}`)}); err == nil {
		t.Fatal("a send for a forgotten task was accepted")
	}
}
