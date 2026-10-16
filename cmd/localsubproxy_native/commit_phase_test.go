package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/durable"
)

// Review2 #4 fault injection at each commit phase of identity.json.
//
//	notCommitted: the write fails before the rename (file untouched)
//	syncFailed:   renamed, directory sync fails, read-back shows the new file
//	uncertain:    renamed, directory sync fails, read-back fails too
type phase int

const (
	notCommitted phase = iota
	syncFailed
	uncertain
)

func injectPhase(t *testing.T, p phase) func() {
	t.Helper()
	ow, or := writeJSON, readFile
	writeJSON = func(f string, v any) error {
		if p == notCommitted {
			return &durable.WriteError{Phase: "rename", Err: errors.New("disk full (injected)")}
		}
		if err := ow(f, v); err != nil {
			t.Fatal(err)
		}
		return &durable.WriteError{Phase: "dirsync", Committed: true, Err: errors.New("dir fsync EIO (injected)")}
	}
	if p == uncertain {
		readFile = func(f string) ([]byte, error) {
			if strings.HasSuffix(f, "identity.json") {
				return nil, &os.PathError{Op: "read", Path: f, Err: syscall.EIO}
			}
			return or(f)
		}
	}
	return func() { writeJSON, readFile = ow, or }
}

func TestRevokeCommitPhases(t *testing.T) {
	for _, c := range []struct {
		name  string
		p     phase
		ok    bool
		left  int  // bindings on disk afterwards
		alive bool // b_a still served
		key   bool // b_a key file still there
	}{
		{"not committed", notCommitted, false, 2, true, true},
		{"sync failed, verified", syncFailed, true, 1, false, false},
		{"uncertain", uncertain, false, 1, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := setupBindings(t, bA, bB)
			runtime.storageWarn = ""
			restore := injectPhase(t, c.p)
			r := do(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"})
			restore()
			if r.OK != c.ok {
				t.Fatalf("ok=%v: %+v", r.OK, r)
			}
			if n := len(onDisk(t, dir).Bindings); n != c.left {
				t.Fatalf("disk bindings %d", n)
			}
			alive := false
			for _, id := range runtime.core.ConnectionIDs() {
				alive = alive || id == "b_a"
			}
			if c.p == uncertain {
				// not restored: it must not take new tasks
				if err := runtime.core.BeginRemove("b_a"); err != nil {
					t.Fatalf("link not left closing: %v", err)
				}
				alive = false
			}
			if alive != c.alive {
				t.Fatalf("b_a served=%v", alive)
			}
			_, err := os.Stat(filepath.Join(dir, bindingKeyFile("b_a")))
			if (err == nil) != c.key {
				t.Fatalf("key present=%v", err == nil)
			}
			if c.p == syncFailed && runtime.storageWarn == "" {
				t.Fatal("storage warning not set")
			}
			if c.p == uncertain { // a retry once storage works completes it
				must(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"})
				if len(runtime.core.ConnectionIDs()) != 1 || len(onDisk(t, dir).Bindings) != 1 {
					t.Fatal("retry after uncertain did not finish")
				}
				if _, err := os.Stat(filepath.Join(dir, bindingKeyFile("b_a"))); !os.IsNotExist(err) {
					t.Fatal("key not deleted on retry")
				}
			}
		})
	}
}

func TestPairingCommitPhases(t *testing.T) {
	s := newTestSaaS(t, "acme")
	for _, c := range []struct {
		name  string
		p     phase
		state string
		keys  int // key dirs left under bindings/
	}{
		{"not committed", notCommitted, "failed", 0},
		{"sync failed, verified", syncFailed, "paired", 1},
		{"uncertain", uncertain, "failed", 1}, // key kept: identity.json may reference it
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			startKernel(t, dir)
			defer must(t, map[string]any{"action": "stop"})
			restore := injectPhase(t, c.p)
			st := pairOnce(t, s, []string{"*"})
			restore()
			if st["state"] != c.state {
				t.Fatalf("pair %v", st)
			}
			if ents, _ := os.ReadDir(filepath.Join(dir, "bindings")); len(ents) != c.keys {
				t.Fatalf("key dirs %d", len(ents))
			}
			if c.p == uncertain {
				// restart: the disk decides; the binding is there and works
				must(t, map[string]any{"action": "stop"})
				startKernel(t, dir)
				if n := len(conns(t)); n != 1 {
					t.Fatalf("after restart %d connections", n)
				}
			}
		})
	}
}

// Pause applied after the save fails, and so does the rollback write: the
// error says so, runtime.local matches disk, and the next start applies it.
func TestPauseRollbackWriteFailure(t *testing.T) {
	dir := setupBindings(t, bA)
	op := setPaused
	setPaused = func(*connector.Connector, string, bool) error { return errors.New("apply failed (injected)") }
	ow := writeJSON
	n := 0
	writeJSON = func(f string, v any) error {
		if n++; n == 1 {
			return ow(f, v) // the pause itself is saved
		}
		return &durable.WriteError{Phase: "rename", Err: errors.New("disk full (injected)")}
	}
	r := do(t, map[string]any{"action": "connection_disconnect", "connection_id": "b_a"})
	writeJSON, setPaused = ow, op
	if r.OK || !strings.Contains(r.Error, "rolling back") || !strings.Contains(r.Error, "next start") {
		t.Fatalf("rollback failure not explicit: %+v", r)
	}
	if !onDisk(t, dir).Bindings[0].Paused || !runtime.local.Bindings[0].Paused {
		t.Fatal("runtime.local does not match disk")
	}
	// a successful rollback restores both
	setPaused = func(*connector.Connector, string, bool) error { return errors.New("apply failed (injected)") }
	r = do(t, map[string]any{"action": "connection_connect", "connection_id": "b_a"})
	setPaused = op
	if r.OK || onDisk(t, dir).Bindings[0].Paused != true || runtime.local.Bindings[0].Paused != true {
		t.Fatalf("rollback did not restore: %+v", r)
	}
}

// Review3 #2a: after an uncertain revoke of A whose file did drop A, a
// policy change on B reconciles from disk first and never writes A back.
func TestUncertainRevokeThenPolicyOnOther(t *testing.T) {
	dir := setupBindings(t, bA, bB)
	restore := injectPhase(t, uncertain)
	if r := do(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"}); r.OK {
		t.Fatal("uncertain revoke reported success")
	}
	restore()
	must(t, map[string]any{"action": "connection_policy_set", "connection_id": "b_b", "policy": map[string]any{"allow": []string{"x/*"}}})
	d := onDisk(t, dir)
	if len(d.Bindings) != 1 || d.Bindings[0].ID != "b_b" || d.Bindings[0].Policy.Allow[0] != "x/*" {
		t.Fatalf("A written back or B lost: %+v", d.Bindings)
	}
	if ids := runtime.core.ConnectionIDs(); len(ids) != 1 || ids[0] != "b_b" {
		t.Fatalf("A still served: %v", ids)
	}
	r := must(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"})
	if r["removed"] != true {
		t.Fatalf("re-revoke of landed A: %v", r)
	}
}

// Review3 #2b: while disk cannot be read back, every change is blocked.
func TestUncertainBlocksChangesUntilReadable(t *testing.T) {
	dir := setupBindings(t, bA, bB)
	restore := injectPhase(t, uncertain)
	do(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"})
	ow := writeJSON
	writes := 0
	writeJSON = func(f string, v any) error { writes++; return ow(f, v) }
	r := do(t, map[string]any{"action": "connection_policy_set", "connection_id": "b_b", "policy": map[string]any{"allow": []string{}}})
	writeJSON = ow
	restore()
	if r.OK || !strings.Contains(r.Error, "blocked") || writes != 0 {
		t.Fatalf("change not blocked: %+v writes=%d", r, writes)
	}
	if len(onDisk(t, dir).Bindings) != 1 {
		t.Fatal("disk rewritten")
	}
}

// Review3 #2c: the uncertain revoke did NOT land (file unchanged); a retry
// that fails before writing must not reopen A.
func TestUncertainRevokeRetryFailureKeepsClosed(t *testing.T) {
	dir := setupBindings(t, bA, bB)
	ow, or := writeJSON, readFile
	writeJSON = func(string, any) error { // nothing written, sync "failed"
		return &durable.WriteError{Phase: "dirsync", Committed: true, Err: errors.New("EIO (injected)")}
	}
	readFile = func(f string) ([]byte, error) {
		if strings.HasSuffix(f, "identity.json") {
			return nil, syscall.EIO
		}
		return or(f)
	}
	do(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"})
	readFile = or // disk readable again: still lists A
	writeJSON = func(string, any) error {
		return &durable.WriteError{Phase: "rename", Err: errors.New("disk full (injected)")}
	}
	r := do(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"})
	writeJSON = ow
	if r.OK {
		t.Fatal("retry should fail")
	}
	if !runtime.core.Closing("b_a") {
		t.Fatal("A reopened after a failed retry")
	}
	if len(onDisk(t, dir).Bindings) != 2 {
		t.Fatal("disk changed")
	}
	must(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"})
	if len(onDisk(t, dir).Bindings) != 1 || runtime.core.Closing("b_a") {
		t.Fatal("final revoke did not complete")
	}
}

// Review3 #3: v1 migration whose identity.json rename succeeded but the
// directory sync failed must keep the key the v2 file references.
func TestMigrateV1RenameOkDirSyncFails(t *testing.T) {
	for _, verifiable := range []bool{true, false} {
		dir := t.TempDir()
		dk := strings.Repeat("ab", 32)
		v1, _ := json.Marshal(localIdentity{DataKey: dk, DeviceID: "dev_1", RemoteURL: "https://saas.example"})
		_ = os.WriteFile(filepath.Join(dir, "identity.json"), v1, 0o600)
		c, _ := durable.NewCipher(dk)
		key, _ := c.Seal(fakeIdentity("https://gw.example", "acme", "dev_1"))
		_ = os.WriteFile(filepath.Join(dir, deviceKeyFile), key, 0o600)
		ow, or := writeJSON, readFile
		writeJSON = func(f string, v any) error {
			if err := ow(f, v); err != nil {
				t.Fatal(err)
			}
			return &durable.WriteError{Phase: "dirsync", Committed: true, Err: errors.New("EIO (injected)")}
		}
		if !verifiable {
			readFile = func(f string) ([]byte, error) {
				if strings.HasSuffix(f, "identity.json") {
					return nil, syscall.EIO
				}
				return or(f)
			}
		}
		_, err := loadIdentity(dir)
		writeJSON, readFile = ow, or
		if verifiable != (err == nil) {
			t.Fatalf("verifiable=%v err=%v", verifiable, err)
		}
		// whatever happened, the next start has a usable binding
		id, err := loadIdentity(dir)
		if err != nil || len(id.Bindings) != 1 {
			t.Fatalf("next start: %v %+v", err, id.Bindings)
		}
		if _, err := loadBinding(dir, id, id.Bindings[0]); err != nil {
			t.Fatalf("v2 references a deleted key: %v", err)
		}
	}
}

// Review4 D: reconcile applies every difference the disk holds, not only
// deletions: policy and pause changes from an uncertain write that landed.
func TestReconcileAppliesPolicyAndPause(t *testing.T) {
	dir := setupBindings(t, bA, bB)
	restore := injectPhase(t, uncertain)
	do(t, map[string]any{"action": "connection_policy_set", "connection_id": "b_b", "policy": map[string]any{"allow": []string{"x/*"}, "max_concurrency": 3}})
	restore()
	if p, _ := runtime.core.Policy("b_b"); len(p.Allow) == 1 && p.Allow[0] == "x/*" {
		t.Fatal("fixture: policy should not be applied yet")
	}
	restore = injectPhase(t, uncertain)
	_ = restore // the pause below is blocked (still uncertain + unreadable)
	if r := do(t, map[string]any{"action": "connection_disconnect", "connection_id": "b_a"}); r.OK {
		t.Fatal("change allowed while unreadable")
	}
	restore()
	// disk readable: hand-edit the pause in (as if an uncertain pause landed)
	d := onDisk(t, dir)
	d.Bindings[0].Paused = true
	if err := writeJSON(filepath.Join(dir, "identity.json"), d); err != nil {
		t.Fatal(err)
	}
	// any change now reconciles first
	must(t, map[string]any{"action": "connection_policy_set", "connection_id": "b_b", "policy": map[string]any{"max_concurrency": 2}})
	p, _ := runtime.core.Policy("b_b")
	if len(p.Allow) != 1 || p.Allow[0] != "x/*" || p.MaxConcurrency != 2 {
		t.Fatalf("policy from disk not applied: %+v", p)
	}
	for _, c := range conns(t) {
		m := c.(map[string]any)
		if m["id"] == "b_a" && m["state"] != "disconnected" {
			t.Fatalf("pause from disk not applied: %v", m)
		}
	}
}

// Review4 D: an uncertain pairing that did land is started by reconcile
// (no restart needed).
func TestReconcileStartsLandedPairing(t *testing.T) {
	s := newTestSaaS(t, "acme")
	s2 := newTestSaaS(t, "beta")
	dir := t.TempDir()
	startKernel(t, dir)
	defer must(t, map[string]any{"action": "stop"})
	restore := injectPhase(t, uncertain)
	if st := pairOnce(t, s, []string{"*"}); st["state"] != "failed" {
		t.Fatalf("pair %v", st)
	}
	restore()
	if len(conns(t)) != 0 {
		t.Fatal("fixture: not running yet")
	}
	// the next change (a new pairing) reconciles first
	if st := pairOnce(t, s2, []string{"*"}); st["state"] != "paired" {
		t.Fatalf("second pairing %v", st)
	}
	if n := len(conns(t)); n != 2 || len(onDisk(t, dir).Bindings) != 2 {
		t.Fatalf("landed pairing not started: %d", n)
	}
}
