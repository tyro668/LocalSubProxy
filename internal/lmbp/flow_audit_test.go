package lmbp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// Audit finding 2: under concurrent senders the bytes unacknowledged by the
// peer must never exceed the negotiated windows (262,144 bytes here).
func TestOutboxNeverExceedsWindowUnderConcurrency(t *testing.T) {
	const window = 262144
	o := NewOutbox()
	o.SetFlow(&Flow{TaskWindow: window, ConnWindow: window})
	var mu sync.Mutex
	written := map[string][]int64{} // task → written seqs not yet acked
	sizes := map[string]map[int64]int{}
	perTask, conn, maxTask, maxConn := map[string]int{}, 0, 0, 0
	o.Attach(func(raw []byte) error {
		var e Envelope
		_ = json.Unmarshal(raw, &e)
		mu.Lock()
		defer mu.Unlock()
		written[e.TaskID] = append(written[e.TaskID], e.Seq)
		if sizes[e.TaskID] == nil {
			sizes[e.TaskID] = map[int64]int{}
		}
		sizes[e.TaskID][e.Seq] = len(raw)
		perTask[e.TaskID] += len(raw)
		conn += len(raw)
		maxTask, maxConn = max(maxTask, perTask[e.TaskID]), max(maxConn, conn)
		return nil
	})
	done := make(chan struct{})
	go func() { // the peer: acks everything written so far, a little late
		for {
			select {
			case <-done:
				return
			case <-time.After(3 * time.Millisecond):
			}
			mu.Lock()
			type ack struct {
				id  string
				seq int64
			}
			var acks []ack
			for id, seqs := range written {
				if len(seqs) == 0 {
					continue
				}
				hi := seqs[0]
				for _, s := range seqs {
					hi = max(hi, s)
				}
				for _, s := range seqs {
					perTask[id] -= sizes[id][s]
					conn -= sizes[id][s]
				}
				written[id] = nil
				acks = append(acks, ack{id, hi})
			}
			mu.Unlock()
			for _, a := range acks {
				o.Ack(a.id, a.seq)
			}
		}
	}()
	payload := strings.Repeat("x", 20000)
	var wg sync.WaitGroup
	for task := 0; task < 4; task++ {
		id := "task_" + string(rune('a'+task))
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					b, _ := json.Marshal(TaskDelta{Text: payload})
					if _, err := o.Send(context.Background(), Envelope{Type: TypeTaskDelta, TaskID: id, Data: b}); err != nil {
						t.Error(err)
						return
					}
					o.mu.Lock() // the sender's own ledger must respect the windows too
					tu, cu := o.task(id).used, o.connUsed
					o.mu.Unlock()
					mu.Lock()
					maxTask, maxConn = max(maxTask, tu), max(maxConn, cu)
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	close(done)
	t.Logf("max unacked per task %d, per connection %d (window %d)", maxTask, maxConn, window)
	if maxTask > window || maxConn > window {
		t.Fatalf("window exceeded: per task %d, per connection %d > %d", maxTask, maxConn, window)
	}
}
