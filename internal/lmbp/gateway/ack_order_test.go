package gateway

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

// v2.1.1 macOS CI (conformance S3): the client cancels, the device's late
// delta (seq 2) is counted by the local finish, and the device's terminal
// frame (seq 3) is ACKed by the read loop before the local finish writes
// its own ACK. The ACK for 2 then reached the wire after the ACK for 3 and
// looked like an ACK for the frame sent after the terminal one. ACKs on the
// wire must be strictly increasing per task.
func TestLateAckNeverFollowsHigherAck(t *testing.T) {
	r := newRig(t, lmbp.Flow{TaskWindow: lmbp.DefaultTaskWindow, ConnWindow: lmbp.DefaultConnWindow}, 0, time.Millisecond)
	r.stop()
	<-r.done
	dev := r.rawHello(t, "")
	waitOnline(t, r)

	var mu sync.Mutex
	handled := map[int64]chan struct{}{2: make(chan struct{}), 3: make(chan struct{})}
	h := func(in lmbp.Envelope) func() {
		mu.Lock()
		ch := handled[in.Seq]
		mu.Unlock()
		if ch == nil {
			return nil
		}
		return func() { close(ch) }
	}
	deviceDataHook.Store(&h)
	defer deviceDataHook.Store(nil)

	var taskID string
	var wmu sync.Mutex // the test goroutine and the gateway's hook both write
	send := func(typ string, seq int64, body any) {
		wmu.Lock()
		defer wmu.Unlock()
		b, _ := json.Marshal(body)
		if err := dev.c.WriteJSON(lmbp.Envelope{V: lmbp.Version, Type: typ, TaskID: taskID, Seq: seq, Data: b}); err != nil {
			t.Error(err)
		}
	}
	wait := func(ch chan struct{}, what string) {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Errorf("%s never handled", what)
		}
	}
	var fired sync.Once
	ran := false
	raceHook = func(gt *gtask, point string) {
		if gt.id != taskID {
			return
		}
		switch point {
		case "finish-transition": // decided locally, task.cancel sent
			send(lmbp.TypeTaskDelta, 2, lmbp.TaskDelta{Text: "late"})
			wait(handled[2], "late delta")
		case "finish-ack": // seq 2 counted, its ACK not written yet
			fired.Do(func() {
				ran = true
				send(lmbp.TypeTaskError, 3, lmbp.ErrorBody{Code: "CANCELLED", Message: "cancelled"})
				wait(handled[3], "terminal frame")
			})
		}
	}
	defer func() { raceHook = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		req := r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("echo"),
			"messages": []map[string]string{{"role": "user", "content": "go"}}}, nil).WithContext(ctx)
		if resp, err := httpc.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	var acks []int64
	next := func(d time.Duration) (lmbp.Envelope, bool) {
		select {
		case e, ok := <-dev.frames:
			return e, ok
		case <-time.After(d):
			return lmbp.Envelope{}, false
		}
	}
	for taskID == "" {
		e, ok := next(5 * time.Second)
		if !ok {
			t.Fatal("no task.run")
		}
		if e.Type == lmbp.TypeTaskRun {
			taskID = e.TaskID
		}
	}
	send(lmbp.TypeTaskDelta, 1, lmbp.TaskDelta{Text: "x"})
	time.Sleep(300 * time.Millisecond)
	cancel()
	for {
		e, ok := next(2 * time.Second)
		if !ok {
			break
		}
		if e.TaskID == taskID && e.Type == lmbp.TypeTaskAck {
			var a lmbp.TaskAck
			_ = json.Unmarshal(e.Data, &a)
			acks = append(acks, a.Seq)
		}
	}
	if !ran {
		t.Fatal("race window was not exercised")
	}
	for i := 1; i < len(acks); i++ {
		if acks[i] <= acks[i-1] {
			t.Fatalf("ACKs on the wire not increasing: %v", acks)
		}
	}
	if len(acks) == 0 || acks[len(acks)-1] != 3 {
		t.Fatalf("ACKs %v: want the last one to be 3 (the terminal frame)", acks)
	}
	// a frame after the terminal one: task.cancel unknown_task, no ACK
	send(lmbp.TypeTaskDelta, 4, lmbp.TaskDelta{Text: "after terminal"})
	var reason string
	for {
		e, ok := next(1500 * time.Millisecond)
		if !ok {
			break
		}
		if e.TaskID != taskID {
			continue
		}
		switch e.Type {
		case lmbp.TypeTaskAck:
			t.Fatalf("frame after the terminal one was ACKed: %s", e.Data)
		case lmbp.TypeTaskCancel:
			var c lmbp.TaskCancel
			_ = json.Unmarshal(e.Data, &c)
			reason = c.Reason
		}
	}
	if reason != "unknown_task" {
		t.Fatalf("after terminal: cancel reason %q, want unknown_task", reason)
	}
}

func waitOnline(t *testing.T, r *rig) {
	t.Helper()
	for i := 0; ; i++ {
		r.g.mu.Lock()
		ds := r.g.devs[r.devID]
		r.g.mu.Unlock()
		if ds != nil && ds.connected() {
			return
		}
		if i > 250 {
			t.Fatal("raw device never came online")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
