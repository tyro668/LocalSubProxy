package store

import (
	"bytes"
	"context"
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
