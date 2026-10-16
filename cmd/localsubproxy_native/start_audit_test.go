package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyro668/LocalSubProxy/internal/connector"
)

// Review7 #17: start(A) whose core build fails must not leak A's audit log:
// a following start(B) has to write to B's own audit.db.
func TestStartCoreFailureReleasesAudit(t *testing.T) {
	call(`{"action":"stop"}`)
	a, b := t.TempDir(), t.TempDir()
	orig := buildCore
	buildCore = func(string, localIdentity) (*connector.Connector, error) {
		return nil, errors.New("core build failed (injected)")
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(start(`{"data_dir":"`+a+`"}`)), &out)
	buildCore = orig
	if out["ok"] == true {
		t.Fatalf("start A should fail: %v", out)
	}
	if runtime.audit != nil {
		t.Fatal("audit log of failed start A still held")
	}
	_ = json.Unmarshal([]byte(start(`{"data_dir":"`+b+`"}`)), &out)
	defer call(`{"action":"stop"}`)
	if out["ok"] != true {
		t.Fatalf("start B: %v", out)
	}
	if _, err := os.Stat(filepath.Join(b, "audit.db")); err != nil {
		t.Fatalf("start B did not open its own audit.db: %v", err)
	}
}
