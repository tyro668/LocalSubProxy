package lmbp

import (
	"context"
	"testing"
)

// Review7 #5: an old connection detaching late must not clear the sender
// a newer connection installed.
func TestDetachIsGenerationOwned(t *testing.T) {
	o := NewOutbox()
	var a, b int
	ga := o.AttachOwned(func([]byte) error { a++; return nil })
	_ = ga
	gb := o.AttachOwned(func([]byte) error { b++; return nil })
	o.Detach(ga) // the old connection's deferred cleanup runs late
	if _, err := o.Send(context.Background(), Envelope{Type: TypeTaskDelta, TaskID: "t", Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if a != 0 || b != 1 {
		t.Fatalf("writes old=%d new=%d", a, b)
	}
	o.Detach(gb)
	if _, err := o.Send(context.Background(), Envelope{Type: TypeTaskDelta, TaskID: "t", Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if b != 1 {
		t.Fatal("detached sender still written")
	}
}
