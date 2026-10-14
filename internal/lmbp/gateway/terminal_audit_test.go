package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// Third audit #1: the terminal event and the terminal task state are one
// atomic write. Scenario: the done event can be written but the status update
// fails; the caller gets INTERNAL; after a restart, a retry with the same
// Idempotency-Key must get the same outcome (never a success), and storage
// must hold exactly one terminal event with a matching state.

func termChat(t *testing.T, base, key, text string) (int, string, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"model": "echo", "messages": user(text)})
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+tenantKey)
	req.Header.Set("X-End-User", "alice")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp := doReq(t, req)
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, resp.Header.Get("X-Task-Id"), sb.String()
}

// checkSingleTerminal asserts one terminal event, unique seqs, and a task
// state that agrees with the terminal event.
func checkSingleTerminal(t *testing.T, st store.Store, id string) (kind string) {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	n := 0
	for _, e := range evs {
		if seen[e.Seq] {
			t.Fatalf("seq %d stored twice", e.Seq)
		}
		seen[e.Seq] = true
		if isTerminal(e.Kind) {
			n++
			kind = e.Kind
		}
	}
	tk, err := st.GetTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d terminal events stored (state %s): %+v", n, tk.State, evs)
	}
	want := map[string]bool{lmbp.TypeTaskDone: tk.State == "SUCCEEDED", lmbp.TypeTaskError: tk.State == "FAILED" || tk.State == "CANCELLED"}
	if !want[kind] {
		t.Fatalf("terminal event %s disagrees with task state %s", kind, tk.State)
	}
	return kind
}

func outcome(code int, body string) string {
	if code == 200 && strings.Contains(body, "[DONE]") || code == 200 && !strings.Contains(body, "error") {
		return "success"
	}
	for _, c := range []string{lmbp.ErrInternal, lmbp.ErrOutcomeUnknown} {
		if strings.Contains(body, c) {
			return c
		}
	}
	return "other:" + body
}

func TestTerminalAtomicSQLiteRestart(t *testing.T) {
	r := newRig(t, lmbp.Flow{}, 0, 0)
	raw, err := sql.Open("sqlite", "file:"+r.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Fault injection inside the database: the SUCCEEDED status update fails.
	if _, err := raw.Exec(`CREATE TRIGGER fail_success BEFORE UPDATE OF state ON tasks WHEN NEW.state='SUCCEEDED' BEGIN SELECT RAISE(ABORT,'injected status write failure'); END`); err != nil {
		t.Fatal(err)
	}
	code1, id, body1 := termChat(t, r.srv.URL, "k-terminal", "a b")
	if code1 == 200 && !strings.Contains(body1, lmbp.ErrInternal) {
		t.Fatalf("status write failed but the caller got success: %d %s", code1, body1)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := raw.Exec(`DROP TRIGGER fail_success`); err != nil {
		t.Fatal(err)
	}

	// Restart: a new gateway over the same database file.
	st2, err := store.OpenSQLite(r.dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	g2, err := New(Config{Store: st2, Logger: log.New(r.logs, "", 0), AttachGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_ = g2.RegisterTenant(context.Background(), "acme", "Acme", tenantKey)
	srv2 := httptest.NewServer(g2.Handler())
	defer srv2.Close()
	code2, id2, body2 := termChat(t, srv2.URL, "k-terminal", "a b")
	if id2 != id {
		t.Fatalf("retry served task %q, want %q", id2, id)
	}
	if o1, o2 := outcome(code1, body1), outcome(code2, body2); o1 != o2 || o2 == "success" {
		t.Fatalf("one idempotent request, two outcomes: before restart %s (%d %s), after %s (%d %s)", o1, code1, body1, o2, code2, body2)
	}
	if k := checkSingleTerminal(t, st2, id); k != lmbp.TypeTaskError {
		t.Fatalf("stored terminal %s, want task.error", k)
	}
}

// onceStore fails the first terminal status write (memory store variant).
type onceStore struct {
	store.Store
	armed atomic.Bool
}

func (o *onceStore) SetTaskState(ctx context.Context, id, state string) error {
	if state == "SUCCEEDED" && o.armed.CompareAndSwap(true, false) {
		return errDisk
	}
	return o.Store.SetTaskState(ctx, id, state)
}

func TestTerminalAtomicMemoryReplay(t *testing.T) {
	var ost *onceStore
	r := newRigWith(t, lmbp.Flow{}, 0, 0, func(store.Store) store.Store { ost = &onceStore{Store: store.NewMemory()}; return ost })
	ost.armed.Store(true)
	code1, id, body1 := termChat(t, r.srv.URL, "k-mem", "a b")
	time.Sleep(300 * time.Millisecond)
	r.g.mu.Lock()
	delete(r.g.tasks, id) // drop the hub: the replay is served from the store
	r.g.mu.Unlock()
	code2, _, body2 := termChat(t, r.srv.URL, "k-mem", "a b")
	if o1, o2 := outcome(code1, body1), outcome(code2, body2); o1 != o2 || o2 == "success" {
		t.Fatalf("one idempotent request, two outcomes: first %s (%d %s), replay %s (%d %s)", o1, code1, body1, o2, code2, body2)
	}
	checkSingleTerminal(t, ost.Store, id)
}

// Recovery decides from a consistent terminal state: a legacy row holding a
// done event but a non-SUCCEEDED state is reported as OUTCOME_UNKNOWN, never
// replayed as success, and nothing is written.
func TestRecoveryInconsistentLegacyRow(t *testing.T) {
	m := store.NewMemory()
	ctx := context.Background()
	_ = m.CreateTask(ctx, store.Task{ID: "task_x", TenantID: "acme", OwnerID: "alice", State: "FAILED"})
	_ = m.AppendEvent(ctx, store.Event{TaskID: "task_x", Seq: 1, Kind: lmbp.TypeTaskDelta, Data: []byte(`{"content":"hi"}`)})
	_ = m.AppendEvent(ctx, store.Event{TaskID: "task_x", Seq: 2, Kind: lmbp.TypeTaskDone, Data: []byte(`{"finish_reason":"stop"}`)})
	g, _ := New(Config{Store: m})
	st, _ := m.GetTask(ctx, "task_x")
	gt, err := g.finishedTask(st, "echo")
	if err != nil {
		t.Fatal(err)
	}
	last := gt.events[len(gt.events)-1]
	if last.Type != lmbp.TypeTaskError || !strings.Contains(string(last.Data), lmbp.ErrOutcomeUnknown) {
		t.Fatalf("inconsistent row replayed as %s %s", last.Type, last.Data)
	}
	evs, _ := m.ListEvents(ctx, "task_x")
	if len(evs) != 2 {
		t.Fatalf("recovery wrote events: %+v", evs)
	}
}

// After the fix the terminal state is only written through FinishTask; the
// injected failure models "event write ok, status write fails" inside it:
// the event insert is rolled back with the failed status update.
func (o *onceStore) FinishTask(ctx context.Context, e store.Event, state string) error {
	if state == "SUCCEEDED" && o.armed.CompareAndSwap(true, false) {
		return errDisk
	}
	return o.Store.FinishTask(ctx, e, state)
}
