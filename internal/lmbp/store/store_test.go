package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEncryptionAndPurge(t *testing.T) {
	ctx := context.Background()
	c, _ := NewCipher(bytes.Repeat([]byte{1}, 32))
	p := filepath.Join(t.TempDir(), "s.db")
	s, err := OpenSQLite(p, c)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = s.CreateTask(ctx, Task{ID: "t_old", State: "SUCCEEDED", Request: []byte("SECRET-PROMPT"), CreatedAt: old, UpdatedAt: old})
	_ = s.AppendEvent(ctx, Event{TaskID: "t_old", Seq: 1, Kind: "task.delta", Data: []byte("SECRET-OUTPUT"), At: old})
	_ = s.CreateTask(ctx, Task{ID: "t_run", State: "RUNNING", Request: []byte("x"), CreatedAt: old, UpdatedAt: old})
	got, err := s.GetTask(ctx, "t_old")
	if err != nil || string(got.Request) != "SECRET-PROMPT" {
		t.Fatalf("roundtrip %q %v", got.Request, err)
	}
	ev, _ := s.ListEvents(ctx, "t_old")
	if len(ev) != 1 || string(ev[0].Data) != "SECRET-OUTPUT" {
		t.Fatal("event roundtrip")
	}
	raw, _ := os.ReadFile(p)
	wal, _ := os.ReadFile(p + "-wal")
	if bytes.Contains(append(raw, wal...), []byte("SECRET")) {
		t.Fatal("plaintext found on disk")
	}
	n, err := s.Purge(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purge n=%d err=%v", n, err)
	}
	if _, err := s.GetTask(ctx, "t_old"); err != ErrNotFound {
		t.Fatal("old task survived purge")
	}
	if _, err := s.GetTask(ctx, "t_run"); err != nil {
		t.Fatal("running task purged")
	}
	s.Close()
	// Re-open without a key: encrypted rows must not silently decode.
	s2, _ := OpenSQLite(p, nil)
	defer s2.Close()
	if _, err := s2.GetTask(ctx, "t_run"); err == nil {
		t.Fatal("encrypted row decoded without key")
	}
}

// Audit finding 1: (owner a, key "b|c") and (owner "a|b", key "c") are distinct records.
func TestIdempotencyKeysUnambiguous(t *testing.T) {
	sq, err := OpenSQLite(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	for name, st := range map[string]Store{"memory": NewMemory(), "sqlite": sq} {
		ctx := context.Background()
		if err := st.PutIdempotency(ctx, Idempotency{TenantID: "acme", OwnerID: "a", Key: "b|c", TaskID: "task_a", BodyHash: "h", CreatedAt: time.Now()}); err != nil {
			t.Fatal(name, err)
		}
		if _, err := st.GetIdempotency(ctx, "acme", "a|b", "c"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: (a|b, c) collided with (a, b|c): %v", name, err)
		}
		if err := st.PutIdempotency(ctx, Idempotency{TenantID: "acme", OwnerID: "a|b", Key: "c", TaskID: "task_ab", BodyHash: "h", CreatedAt: time.Now()}); err != nil {
			t.Fatalf("%s: distinct record refused: %v", name, err)
		}
		if i, _ := st.GetIdempotency(ctx, "acme", "a", "b|c"); i.TaskID != "task_a" {
			t.Fatalf("%s: got %+v", name, i)
		}
	}
}
