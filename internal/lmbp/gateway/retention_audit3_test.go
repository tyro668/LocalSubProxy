package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// failRunningStore fails SetTaskState (e.g. the row was purged or the disk failed).
type failRunningStore struct{ store.Store }

func (f failRunningStore) SetTaskState(context.Context, string, string) error {
	return errors.New("disk I/O error")
}

// audit3 P2-4: a SetTaskState failure is handled: the task is never sent to
// the device and the caller gets a storage error. Was: ignored, task ran.
func TestSetTaskStateFailureDoesNotRunTask(t *testing.T) {
	r := newRigWith(t, lmbp.Flow{}, 0, 0, func(s store.Store) store.Store { return failRunningStore{s} })
	resp := doReq(t, r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model("echo"), "messages": user("hi")}, nil))
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	id := resp.Header.Get("X-Task-Id")
	if resp.StatusCode != 500 || errCodeOf(b) != lmbp.ErrInternal {
		t.Fatalf("status %d %s", resp.StatusCode, b)
	}
	time.Sleep(200 * time.Millisecond)
	if n := r.exec.count(id); n != 0 {
		t.Fatalf("task ran %d times although its RUNNING state was not persisted", n)
	}
}

func errCodeOf(b []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	return e.Error.Code
}

// audit3 P2-5: a crash leaves RUNNING/QUEUED rows. On restart the gateway
// persists OUTCOME_UNKNOWN (terminal event + FAILED state) so replays agree
// and retention purges the prompt and output. Was: synthesized on read
// only; the RUNNING row bypassed retention forever.
func TestRestartPersistsOrphanOutcome(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gw.db")
	st, err := store.OpenSQLite(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // Windows cannot remove an open database file
	old := time.Now().Add(-time.Hour)
	for _, s := range []string{"RUNNING", "QUEUED"} {
		id := "task_orphan_" + s
		if err := st.CreateTask(ctx, store.Task{ID: id, TenantID: "acme", OwnerID: "alice", DeviceID: "d", Model: "m", State: s, Request: []byte(`{"secret":"prompt"}`), CreatedAt: old, UpdatedAt: old}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AppendEvent(ctx, store.Event{TaskID: "task_orphan_RUNNING", Seq: 1, Kind: lmbp.TypeTaskDelta, Data: []byte(`{"text":"partial"}`), At: old}); err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	g, err := New(Config{Store: st, Logger: log.New(io.Discard, "", 0), SigningKey: priv, Retention: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"RUNNING", "QUEUED"} {
		id := "task_orphan_" + s
		rec, err := st.GetTask(ctx, id)
		if err != nil || rec.State != "FAILED" {
			t.Fatalf("%s orphan state after restart: %q %v", s, rec.State, err)
		}
		evs, _ := st.ListEvents(ctx, id)
		last := evs[len(evs)-1]
		var eb lmbp.ErrorBody
		_ = json.Unmarshal(last.Data, &eb)
		if last.Kind != lmbp.TypeTaskError || eb.Code != lmbp.ErrOutcomeUnknown || (s == "RUNNING" && last.Seq != 2) {
			t.Fatalf("%s orphan terminal event: %+v %+v", s, last, eb)
		}
	}
	// retention now applies: the prompt and output go
	time.Sleep(10 * time.Millisecond)
	if n := g.Purge(ctx, time.Now().Add(2*time.Minute)); n != 2 {
		t.Fatalf("retention purged %d orphan tasks, want 2", n)
	}
	if _, err := st.GetTask(ctx, "task_orphan_RUNNING"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("orphan still stored after retention: %v", err)
	}
}
