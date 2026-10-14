package gateway

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// failStore makes event / terminal-state writes fail on demand.
type failStore struct {
	store.Store
	failEventSeq atomic.Int64 // AppendEvent fails for this seq (0: never)
	failState    atomic.Bool  // SetTaskState fails for terminal states
}

var errDisk = errors.New("disk full")

func (f *failStore) AppendEvent(ctx context.Context, e store.Event) error {
	if n := f.failEventSeq.Load(); n != 0 && e.Seq == n && e.Kind != lmbp.TypeTaskError {
		return errDisk
	}
	return f.Store.AppendEvent(ctx, e)
}

func (f *failStore) SetTaskState(ctx context.Context, id, state string) error {
	if f.failState.Load() && state != "RUNNING" && state != "QUEUED" {
		return errDisk
	}
	return f.Store.SetTaskState(ctx, id, state)
}

func persistRig(t *testing.T) (*rig, *failStore) {
	var fs *failStore
	r := newRigWith(t, lmbp.Flow{}, 0, 0, func(s store.Store) store.Store { fs = &failStore{Store: s}; return fs })
	return r, fs
}

func (r *rig) taskAcked(t *testing.T, id string) int64 {
	r.g.mu.Lock()
	gt := r.g.tasks[id]
	r.g.mu.Unlock()
	if gt == nil {
		t.Fatalf("task %s not in memory", id)
	}
	gt.mu.Lock()
	defer gt.mu.Unlock()
	return gt.acked
}

func chatOnce(t *testing.T, r *rig, text string) (int, string, string) {
	resp := doReq(t, r.req("POST", "/v1/chat/completions", map[string]any{"model": "echo", "messages": user(text)}, nil))
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, resp.Header.Get("X-Task-Id"), b.String()
}

// Reliability 1a: a device frame whose event write fails must not be ACKed
// and the caller must get an error, not a success built from unpersisted data.
func TestNoAckWithoutPersistedEvent(t *testing.T) {
	r, fs := persistRig(t)
	fs.failEventSeq.Store(2) // second delta of "a b c d"
	code, id, body := chatOnce(t, r, "a b c d")
	if code == 200 || !strings.Contains(body, lmbp.ErrInternal) {
		t.Fatalf("unpersisted output reported as success: %d %s", code, body)
	}
	time.Sleep(200 * time.Millisecond)
	if a := r.taskAcked(t, id); a >= 2 {
		t.Fatalf("gateway ACKed seq %d although seq 2 was never persisted", a)
	}
	st, err := r.g.cfg.Store.GetTask(context.Background(), id)
	if err != nil || st.State == "SUCCEEDED" {
		t.Fatalf("stored state %q %v", st.State, err)
	}
	if !strings.Contains(r.logs.String(), "disk full") {
		t.Fatalf("persistence error not surfaced in logs: %s", r.logs.String())
	}
}

// Reliability 1b: the terminal frame is ACKed only after the terminal state
// is persisted; otherwise the caller sees an error.
func TestNoAckWithoutPersistedTerminalState(t *testing.T) {
	r, fs := persistRig(t)
	fs.failState.Store(true)
	code, id, body := chatOnce(t, r, "x y")
	if code == 200 || !strings.Contains(body, lmbp.ErrInternal) {
		t.Fatalf("unpersisted terminal state reported as success: %d %s", code, body)
	}
	time.Sleep(200 * time.Millisecond)
	evs, _ := r.g.cfg.Store.ListEvents(context.Background(), id)
	var doneSeq int64
	for _, e := range evs {
		if e.Kind == lmbp.TypeTaskDone {
			doneSeq = e.Seq
		}
	}
	if a := r.taskAcked(t, id); doneSeq == 0 && a >= 3 || doneSeq != 0 && a >= doneSeq {
		t.Fatalf("terminal frame ACKed (%d) without persisted terminal state (done seq %d)", a, doneSeq)
	}
}

func (f *failStore) FinishTask(ctx context.Context, e store.Event, state string) error {
	if n := f.failEventSeq.Load(); n != 0 && e.Seq == n && e.Kind != lmbp.TypeTaskError {
		return errDisk
	}
	if f.failState.Load() {
		return errDisk
	}
	return f.Store.FinishTask(ctx, e, state)
}
