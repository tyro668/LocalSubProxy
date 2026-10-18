package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
	"github.com/tyro668/avora/internal/lmbp/store"
)

// Review9 P2-new-2: the default in-memory store's Purge deleted expired tasks
// and events but kept their idempotency records, and the lookup did not
// treat a record pointing at a missing task as absent. A retry with the same
// key and body then tried to re-attach to the purged task and got 404, and
// the records grew without bound.
func TestIdempotencyAfterPurgeMemoryStore(t *testing.T) {
	var mem *store.Memory
	r := newRigWith(t, lmbp.Flow{}, 0, 0, func(store.Store) store.Store { mem = store.NewMemory(); return mem })
	a := idemChat(t, r, "echo", "one two", "k-purge")
	if a.code != 200 || a.task == "" {
		t.Fatalf("first call: %d %s", a.code, a.body)
	}
	// Retention (days) elapsed for this finished task: the in-memory record
	// was dropped by its AttachGrace+10min timer long ago, and RunRetention
	// purges the stored task.
	r.g.mu.Lock()
	delete(r.g.tasks, a.task)
	r.g.mu.Unlock()
	if n, err := mem.Purge(context.Background(), time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}
	if _, err := mem.GetIdempotency(context.Background(), "acme", "alice", "k-purge"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("idempotency record survived the purge of its task: %v", err)
	}
	b := idemChat(t, r, "echo", "one two", "k-purge")
	if b.code != 200 {
		t.Fatalf("retry after purge: %d %s (key pointed at a purged task)", b.code, b.body)
	}
	if b.task == a.task {
		t.Fatalf("retry re-attached to the purged task %s: %d %s", a.task, b.code, b.body)
	}
}
