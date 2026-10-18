package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// audit3 P2-4: retention deletes only explicit terminal records. A task still
// QUEUED (or RUNNING, or in an unknown state) longer than the retention
// period keeps its row and its idempotency record, also past SQLite's 24h
// key rule; was: QUEUED tasks and their keys deleted while still queued.
func TestPurgeKeepsNonTerminalTasks(t *testing.T) {
	ctx := context.Background()
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "s.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	for name, s := range map[string]Store{"memory": NewMemory(), "sqlite": sq} {
		t.Run(name, func(t *testing.T) {
			old := time.Now().Add(-48 * time.Hour)
			states := []string{"QUEUED", "RUNNING", "WEIRD", "SUCCEEDED"}
			for _, st := range states {
				id := "t_" + st
				if err := s.CreateTask(ctx, Task{ID: id, State: st, Request: []byte("x"), CreatedAt: old, UpdatedAt: old},
					&Idempotency{TenantID: "t", OwnerID: "u", Key: "k_" + st, TaskID: id, BodyHash: "h", CreatedAt: old}); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := s.Purge(ctx, time.Now()); err != nil || n != 1 {
				t.Fatalf("purge n=%d err=%v (only the SUCCEEDED task may go)", n, err)
			}
			for _, st := range states[:3] {
				if _, err := s.GetTask(ctx, "t_"+st); err != nil {
					t.Errorf("%s task purged: %v", st, err)
				}
				if i, err := s.GetIdempotency(ctx, "t", "u", "k_"+st); err != nil || i.TaskID != "t_"+st {
					t.Errorf("%s task lost its idempotency record: %+v %v", st, i, err)
				}
			}
		})
	}
}

// SetTaskState on a missing row reports it, so the gateway never runs a task
// whose state cannot be persisted.
func TestSetTaskStateMissingRow(t *testing.T) {
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "s.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	for name, s := range map[string]Store{"memory": NewMemory(), "sqlite": sq} {
		if err := s.SetTaskState(context.Background(), "nope", "RUNNING"); err == nil {
			t.Errorf("%s: SetTaskState on a missing task succeeded", name)
		}
	}
}
