package lmbp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Review7 #13: the outbox checks the final wire bytes and never enqueues an
// oversized frame (no seq consumed, no credit charged).
func TestOutboxRejectsOversizedFrame(t *testing.T) {
	o := NewOutbox()
	o.SetFlow(&Flow{TaskWindow: DefaultTaskWindow, ConnWindow: DefaultConnWindow})
	big := []byte(`"` + strings.Repeat("x", MaxFrameBytes) + `"`)
	if _, err := o.Send(context.Background(), Envelope{Type: TypeTaskDelta, TaskID: "t", Data: big}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized frame: %v", err)
	}
	if frames, bytes := o.Usage(); frames != 0 || bytes != 0 {
		t.Fatalf("frame enqueued: %d frames %d bytes", frames, bytes)
	}
	seq, err := o.Send(context.Background(), Envelope{Type: TypeTaskDelta, TaskID: "t", Data: []byte(`{}`)})
	if err != nil || seq != 1 {
		t.Fatalf("next frame: seq %d err %v", seq, err)
	}
}
