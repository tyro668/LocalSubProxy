package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Review9 P2-new-2: purging a task also purges its idempotency records, so
// there is never a "key exists, task does not" state; a dangling record
// (legacy data) reads as absent and does not block a new task with that key.
func TestPurgeRemovesIdempotencyOfPurgedTasks(t *testing.T) {
	ctx := context.Background()
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "s.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	for name, s := range map[string]Store{"memory": NewMemory(), "sqlite": sq} {
		t.Run(name, func(t *testing.T) {
			old := time.Now().Add(-2 * time.Hour) // recent enough for SQLite's own 24h key rule
			idem := func(task, key string) *Idempotency {
				return &Idempotency{TenantID: "t", OwnerID: "u", Key: key, TaskID: task, BodyHash: "h", CreatedAt: old}
			}
			if err := s.CreateTask(ctx, Task{ID: "t_old", State: "SUCCEEDED", Request: []byte("x"), CreatedAt: old, UpdatedAt: old}, idem("t_old", "k1")); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateTask(ctx, Task{ID: "t_live", State: "SUCCEEDED", Request: []byte("x"), CreatedAt: time.Now(), UpdatedAt: time.Now()}, idem("t_live", "k2")); err != nil {
				t.Fatal(err)
			}
			if n, err := s.Purge(ctx, time.Now().Add(-time.Hour)); err != nil || n != 1 {
				t.Fatalf("purge n=%d err=%v", n, err)
			}
			if _, err := s.GetIdempotency(ctx, "t", "u", "k1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("key of purged task still present: %v", err)
			}
			if i, err := s.GetIdempotency(ctx, "t", "u", "k2"); err != nil || i.TaskID != "t_live" {
				t.Fatalf("key of kept task: %+v %v", i, err)
			}
			if err := s.CreateTask(ctx, Task{ID: "t_new", State: "QUEUED", Request: []byte("x"), CreatedAt: time.Now(), UpdatedAt: time.Now()}, idem("t_new", "k1")); err != nil {
				t.Fatalf("reusing the key of a purged task: %v", err)
			}
		})
	}
}

func TestDanglingIdempotencyReadsAsAbsent(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	m.idem[[3]string{"t", "u", "k"}] = Idempotency{TenantID: "t", OwnerID: "u", Key: "k", TaskID: "gone", BodyHash: "h", CreatedAt: time.Now()}
	if _, err := m.GetIdempotency(ctx, "t", "u", "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dangling record returned: %v", err)
	}
	if err := m.CreateTask(ctx, Task{ID: "t2", State: "QUEUED", Request: []byte("x")}, &Idempotency{TenantID: "t", OwnerID: "u", Key: "k", TaskID: "t2", BodyHash: "h"}); err != nil {
		t.Fatalf("dangling record blocked a new task: %v", err)
	}
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "d.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	if _, err := sq.db.Exec(`INSERT INTO idempotency(tenant_id,owner_id,idem_key,task_id,body_hash,created_at) VALUES('t','u','k','gone','h',?)`, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.GetIdempotency(ctx, "t", "u", "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sqlite dangling record returned: %v", err)
	}
	if err := sq.CreateTask(ctx, Task{ID: "t2", State: "QUEUED", Request: []byte("x")}, &Idempotency{TenantID: "t", OwnerID: "u", Key: "k", TaskID: "t2", BodyHash: "h"}); err != nil {
		t.Fatalf("sqlite dangling record blocked a new task: %v", err)
	}
}
