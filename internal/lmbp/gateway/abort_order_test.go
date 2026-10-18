package gateway

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

// ci-macfix Windows run: an invalid terminal frame makes the gateway cancel
// the task. The real device answers the cancel with task.error CANCELLED
// (seq 2). When that answer was handled before the gateway decided the
// outcome, it won: the task ended as a device cancellation and seq 2 was
// accepted. The outcome must be the gateway's INTERNAL error however fast
// the device answers.
func TestDeviceCancelReplyCannotWinProtocolAbort(t *testing.T) {
	r := newRig(t, lmbp.Flow{TaskWindow: lmbp.DefaultTaskWindow, ConnWindow: lmbp.DefaultConnWindow}, 2, 10*time.Second)
	var mu sync.Mutex
	var taskID string
	replied := make(chan struct{})
	var once sync.Once
	h := func(in lmbp.Envelope) func() {
		mu.Lock()
		mine := in.TaskID == taskID && in.Type == lmbp.TypeTaskError
		mu.Unlock()
		if !mine {
			return nil
		}
		return func() { once.Do(func() { close(replied) }) }
	}
	deviceDataHook.Store(&h)
	defer deviceDataHook.Store(nil)
	waited := false
	raceHook = func(gt *gtask, point string) {
		if point != "abort-cancel-sent" || gt.id != taskID {
			return
		}
		select { // let the device's answer to task.cancel go through first
		case <-replied:
			waited = true
		case <-time.After(5 * time.Second):
		}
	}
	defer func() { raceHook = nil }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("slow"), "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "go"}}}, nil).WithContext(ctx)
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	id := resp.Header.Get("X-Task-Id")
	mu.Lock()
	taskID = id
	mu.Unlock()
	buf := make([]byte, 256)
	_, _ = io.ReadAtLeast(resp.Body, buf, 200)
	r.g.mu.Lock()
	ds := r.g.devs[r.devID]
	r.g.mu.Unlock()
	for ds.in.Recv(id) < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	r.g.onDeviceData(ds, lmbp.Envelope{V: lmbp.Version, Type: lmbp.TypeTaskDone, TaskID: id, Seq: 2, Data: json.RawMessage(`{}`)}, 64)
	if got := ds.in.Recv(id); got >= 2 {
		t.Errorf("seq 2 accepted after the invalid frame: recv %d", got)
	}
	_, _ = io.ReadAll(resp.Body)
	if !waited {
		t.Fatal("the device never answered task.cancel: race window not exercised")
	}
	st, err := r.g.cfg.Store.GetTask(context.Background(), id)
	if err != nil || st.State != "FAILED" {
		t.Fatalf("task state %q (err %v), want FAILED", st.State, err)
	}
	evs, _ := r.g.cfg.Store.ListEvents(context.Background(), id)
	last := evs[len(evs)-1]
	var eb lmbp.ErrorBody
	_ = json.Unmarshal(last.Data, &eb)
	if last.Kind != lmbp.TypeTaskError || eb.Code != lmbp.ErrInternal {
		t.Fatalf("terminal event %s %s, want task.error INTERNAL (the device's CANCELLED answer won)", last.Kind, last.Data)
	}
}
