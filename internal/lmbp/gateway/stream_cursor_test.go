package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

// The stream cursor is found by binary search over the (seq-ordered)
// history: resuming after any seq delivers exactly the later events, and a
// long history is not rescanned per wake-up (was O(n²): the demo's 60k-chunk
// stream took ~160 s under -race).
func TestStreamCursorAndLongHistory(t *testing.T) {
	const n = 200000
	tk := &gtask{g: &Gateway{}, notify: make(chan struct{}), subs: map[int]int64{}, done: true}
	for i := 1; i <= n; i++ {
		tk.events = append(tk.events, event{Seq: int64(i), Type: lmbp.TypeTaskDelta})
	}
	tk.events = append(tk.events, event{Seq: n + 1, Type: lmbp.TypeTaskDone})
	for _, after := range []int64{0, 1, n / 2, n, n + 1} {
		var got []int64
		if err := tk.stream(context.Background(), after, nil, func(e event) error { got = append(got, e.Seq); return nil }); err != nil {
			t.Fatal(err)
		}
		want := int64(n+1) - after
		if int64(len(got)) != want || (want > 0 && (got[0] != after+1 || got[len(got)-1] != n+1)) {
			t.Fatalf("after %d: got %d events", after, len(got))
		}
	}
	// many wake-ups on a long live history stay cheap
	live := &gtask{g: &Gateway{}, notify: make(chan struct{}), subs: map[int]int64{}, events: tk.events[:n]}
	done := make(chan error, 1)
	cnt := 0
	go func() {
		done <- live.stream(context.Background(), n-1, nil, func(e event) error { cnt++; return nil })
	}()
	start := time.Now()
	for i := 0; i < 2000; i++ {
		live.mu.Lock()
		seq := int64(n + i)
		typ := lmbp.TypeTaskDelta
		if i == 1999 {
			typ = lmbp.TypeTaskDone
			live.done = true
		}
		live.events = append(live.events, event{Seq: seq, Type: typ})
		live.broadcastLocked()
		live.mu.Unlock()
	}
	select {
	case err := <-done:
		if err != nil || cnt != 2001 {
			t.Fatalf("live stream: %v, %d events", err, cnt)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("live stream over a long history did not finish")
	}
	t.Logf("2000 wake-ups over %d-event history: %v", n, time.Since(start))
}
