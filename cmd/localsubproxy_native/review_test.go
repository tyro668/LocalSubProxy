package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/durable"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

type testSaaS struct {
	remote, gw, key string
}

func newTestSaaS(t *testing.T, tenant string) *testSaaS {
	t.Helper()
	st, _ := store.OpenSQLite(":memory:", nil)
	t.Cleanup(func() { st.Close() })
	g, err := gateway.New(gateway.Config{Store: st, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	s := &testSaaS{key: strings.Repeat(tenant[:1], 40)}
	_ = g.RegisterTenant(context.Background(), tenant, strings.ToUpper(tenant), s.key)
	gws := httptest.NewServer(g.Handler())
	t.Cleanup(gws.Close)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"gateway_url": gws.URL, "tenant": tenant, "link_url": "http://127.0.0.1/link"})
	}))
	t.Cleanup(remote.Close)
	s.remote, s.gw = remote.URL, gws.URL
	return s
}

func (s *testSaaS) approve(t *testing.T, code string) {
	b, _ := json.Marshal(map[string]string{"user_code": code})
	req, _ := http.NewRequest("POST", s.gw+"/lmbp/v1/tenant/devices/approvals", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("X-End-User", "alice")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
}

type reply struct {
	OK    bool           `json:"ok"`
	Error string         `json:"error"`
	Data  map[string]any `json:"data"`
}

func do(t *testing.T, req map[string]any) reply {
	t.Helper()
	b, _ := json.Marshal(req)
	var r reply
	if err := json.Unmarshal([]byte(call(string(b))), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func must(t *testing.T, req map[string]any) map[string]any {
	t.Helper()
	r := do(t, req)
	if !r.OK {
		t.Fatalf("%v: %s", req["action"], r.Error)
	}
	return r.Data
}

func startKernel(t *testing.T, dir string) {
	t.Helper()
	var r reply
	_ = json.Unmarshal([]byte(start(dataDirJSON(dir))), &r)
	if !r.OK {
		t.Fatal(r.Error)
	}
	t.Cleanup(func() { call(`{"action":"stop"}`) })
}

func conns(t *testing.T) []any {
	t.Helper()
	cs, _ := must(t, map[string]any{"action": "status"})["connections"].([]any)
	return cs
}

func failWrites(t *testing.T) func() {
	orig := writeJSON
	writeJSON = func(string, any) error { return errors.New("disk full (injected)") }
	return func() { writeJSON = orig }
}

// Review #1: approve, then immediately cancel / stop / start another
// pairing. The background Wait must not touch the cleared ps.p (it used to
// nil-panic the native process). Run with -race.
func TestApproveThenCancelStopRestartRace(t *testing.T) {
	s := newTestSaaS(t, "acme")
	dir := t.TempDir()
	startKernel(t, dir)
	// make the background goroutine start after the cancel/stop/new pairing
	pairWaitHook = func() { time.Sleep(100 * time.Millisecond) }
	defer func() { pairWaitHook = func() {} }()
	for i := 0; i < 6; i++ {
		p := must(t, map[string]any{"action": "pair_start", "remote_url": s.remote})
		a := must(t, map[string]any{"action": "pair_approve", "pair_id": p["pair_id"], "allow": []string{}})
		switch i % 3 {
		case 0:
			must(t, map[string]any{"action": "pair_cancel", "pair_id": p["pair_id"]})
		case 1:
			must(t, map[string]any{"action": "stop"})
			startKernel(t, dir)
		case 2:
			must(t, map[string]any{"action": "pair_start", "remote_url": s.remote})
		}
		s.approve(t, a["user_code"].(string)) // a late approval must not create a binding
	}
	time.Sleep(1500 * time.Millisecond)
	if n := len(conns(t)); n != 0 {
		t.Fatalf("cancelled pairings created %d connections", n)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "bindings")); len(ents) != 0 {
		t.Fatalf("key files left: %d", len(ents))
	}
}

func pairOnce(t *testing.T, s *testSaaS, allow []string) map[string]any {
	t.Helper()
	p := must(t, map[string]any{"action": "pair_start", "remote_url": s.remote})
	a := must(t, map[string]any{"action": "pair_approve", "pair_id": p["pair_id"], "allow": allow})
	s.approve(t, a["user_code"].(string))
	var st map[string]any
	for i := 0; i < 100; i++ {
		if st = must(t, map[string]any{"action": "pair_status", "pair_id": p["pair_id"]}); st["state"] != "pending" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return st
}

// Review #3: identity.json cannot be written when the pairing completes →
// the pairing fails, no connection starts, the new key file is removed.
func TestPairingPersistFailureStartsNothing(t *testing.T) {
	s := newTestSaaS(t, "acme")
	dir := t.TempDir()
	startKernel(t, dir)
	restore := failWrites(t)
	st := pairOnce(t, s, []string{"*"})
	restore()
	if st["state"] != "failed" || !strings.Contains(st["error"].(string), "disk full") {
		t.Fatalf("pair status %v", st)
	}
	if n := len(conns(t)); n != 0 || len(runtime.core.ConnectionIDs()) != 0 {
		t.Fatalf("connection started although not persisted: %d", n)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "bindings")); len(ents) != 0 {
		t.Fatal("orphan key file left")
	}
	// and a normal pairing afterwards works and survives a restart
	if st := pairOnce(t, s, []string{"*"}); st["state"] != "paired" {
		t.Fatalf("second pairing %v", st)
	}
	must(t, map[string]any{"action": "stop"})
	startKernel(t, dir)
	if n := len(conns(t)); n != 1 {
		t.Fatalf("after restart %d connections", n)
	}
}

// --- store-level fixtures (no gateway needed) ---

func setupBindings(t *testing.T, bs ...binding) string {
	t.Helper()
	dir := t.TempDir()
	id := localIdentity{Version: 2, DataKey: strings.Repeat("ab", 32)}
	for _, b := range bs {
		if b.KeyFile == "" {
			b.KeyFile = bindingKeyFile(b.ID)
		}
		if err := saveBinding(dir, id, b, fakeIdentity(b.Issuer, b.Tenant, b.DeviceID)); err != nil {
			t.Fatal(err)
		}
		id.Bindings = append(id.Bindings, b)
	}
	if err := writeJSON(filepath.Join(dir, "identity.json"), id); err != nil {
		t.Fatal(err)
	}
	core, err := newCore(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	runtime.local, runtime.core, runtime.dir, runtime.leftovers = id, core, dir, nil
	runtime.uncertain, runtime.keepClosed, runtime.landed = false, nil, nil
	t.Cleanup(func() {
		if runtime.core != nil {
			runtime.core.Shutdown()
		}
		runtime.core = nil
	})
	return dir
}

func onDisk(t *testing.T, dir string) localIdentity {
	t.Helper()
	var id localIdentity
	b, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err := json.Unmarshal(b, &id); err != nil {
		t.Fatal(err)
	}
	return id
}

var bA = binding{ID: "b_a", DeviceID: "dev_a", Issuer: "https://gw.a", Tenant: "a", Policy: policyAll()}
var bB = binding{ID: "b_b", DeviceID: "dev_b", Issuer: "https://gw.b", Tenant: "b", Policy: policyAll()}

// Review #3: a failed write during revoke changes nothing; retry succeeds.
func TestRevokePersistFailureKeepsEverything(t *testing.T) {
	dir := setupBindings(t, bA, bB)
	restore := failWrites(t)
	if r := do(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"}); r.OK {
		t.Fatal("revoke reported success although identity.json was not written")
	}
	restore()
	if len(runtime.local.Bindings) != 2 || len(runtime.core.ConnectionIDs()) != 2 || len(onDisk(t, dir).Bindings) != 2 {
		t.Fatal("state diverged after a failed revoke")
	}
	if _, err := os.Stat(filepath.Join(dir, bindingKeyFile("b_a"))); err != nil {
		t.Fatal("key deleted although the removal was not persisted")
	}
	must(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"}) // AbortRemove released it
	if ids := runtime.core.ConnectionIDs(); len(ids) != 1 || ids[0] != "b_b" || len(onDisk(t, dir).Bindings) != 1 {
		t.Fatalf("after retry %v", ids)
	}
}

// Review #3: pause and policy changes are persisted before they apply.
func TestPauseAndPolicyPersistFailure(t *testing.T) {
	dir := setupBindings(t, bA)
	restore := failWrites(t)
	if r := do(t, map[string]any{"action": "connection_disconnect", "connection_id": "b_a"}); r.OK {
		t.Fatal("pause succeeded without persisting")
	}
	if r := do(t, map[string]any{"action": "connection_policy_set", "connection_id": "b_a", "policy": map[string]any{"allow": []string{}}}); r.OK {
		t.Fatal("policy succeeded without persisting")
	}
	restore()
	c := conns(t)[0].(map[string]any)
	if c["paused"] == true || len(c["allow"].([]any)) != 1 || runtime.local.Bindings[0].Paused {
		t.Fatalf("runtime changed: %v", c)
	}
	if d := onDisk(t, dir).Bindings[0]; d.Paused || len(d.Policy.Allow) != 1 {
		t.Fatalf("disk changed: %+v", d)
	}
	must(t, map[string]any{"action": "connection_disconnect", "connection_id": "b_a"})
	if !onDisk(t, dir).Bindings[0].Paused {
		t.Fatal("pause not persisted")
	}
}

// Review #4: partial policy updates keep the other fields (concurrency,
// wildcards and entries for models that are currently disabled).
func TestPolicyPartialUpdate(t *testing.T) {
	b := bA
	b.Policy.Allow, b.Policy.MaxConcurrency = []string{"*"}, 2
	dir := setupBindings(t, b)
	must(t, map[string]any{"action": "connection_policy_set", "connection_id": "b_a", "policy": map[string]any{"allow": []string{"ollama/*", "codex/gpt-5"}}})
	if p := onDisk(t, dir).Bindings[0].Policy; p.MaxConcurrency != 2 || len(p.Allow) != 2 || p.Allow[0] != "ollama/*" {
		t.Fatalf("allow-only update lost fields: %+v", p)
	}
	must(t, map[string]any{"action": "connection_policy_set", "connection_id": "b_a", "policy": map[string]any{"max_concurrency": 3}})
	p := onDisk(t, dir).Bindings[0].Policy
	if p.MaxConcurrency != 3 || len(p.Allow) != 2 || p.Allow[1] != "codex/gpt-5" {
		t.Fatalf("concurrency-only update lost the allow-list: %+v", p)
	}
	if live, _ := runtime.core.Policy("b_a"); live.MaxConcurrency != 3 || len(live.Allow) != 2 {
		t.Fatalf("live policy %+v", live)
	}
}

// Gap (b) / Review2 #2: a key deletion failure is reported (connection
// removed + cleanup failed), persisted as pending_cleanup (incl. the root
// v1 backup of a migrated binding), retried at every start, and the warning
// stays until a retry succeeds.
func TestKeyDeleteFailurePersistedAndRetried(t *testing.T) {
	b := bA
	b.Migrated = true
	dir := setupBindings(t, b)
	bak := filepath.Join(dir, deviceKeyFile+".v1.bak")
	_ = os.WriteFile(bak, []byte("old key"), 0o600)
	origAll, origOne := removeAll, removeOne
	fail := func(string) error { return errors.New("permission denied (injected)") }
	removeAll, removeOne = fail, fail
	defer func() { removeAll, removeOne = origAll, origOne }()
	r := must(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"})
	if r["removed"] != true || len(r["connections"].([]any)) != 0 {
		t.Fatalf("connection not removed: %v", r)
	}
	if w, _ := r["warning"].(string); !strings.Contains(w, "permission denied") || len(r["cleanup_errors"].([]any)) != 3 {
		t.Fatalf("cleanup failure not reported: %v", r)
	}
	want := []string{filepath.Join("bindings", "b_a"), deviceKeyFile + ".v1.bak", deviceKeyFile}
	if p := onDisk(t, dir).PendingCleanup; !slices.Equal(p, want) {
		t.Fatalf("pending not persisted: %v", p)
	}
	if runtime.cancel == nil {
		runtime.cancel = func() {}
	}
	restart := func() map[string]any {
		call(`{"action":"stop"}`)
		var out map[string]any
		_ = json.Unmarshal([]byte(start(dataDirJSON(dir))), &out)
		st, _ := out["data"].(map[string]any)
		if st == nil {
			t.Fatalf("start: %v", out)
		}
		return st
	}
	// still failing at the next start: the warning is kept
	if st := restart(); st["warning"] == nil || len(st["cleanup_pending"].([]any)) != 3 {
		t.Fatalf("warning dropped while cleanup still pending: %v", st)
	}
	removeAll, removeOne = origAll, origOne
	st := restart()
	if st["warning"] != nil || len(st["cleanup_pending"].([]any)) != 0 || len(onDisk(t, dir).PendingCleanup) != 0 {
		t.Fatalf("retry did not clear: %v", st)
	}
	for _, f := range []string{filepath.Join(dir, "bindings", "b_a"), bak} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s still exists", f)
		}
	}
	call(`{"action":"stop"}`)
}

// Gap (c): a v1 install whose device_key.enc is missing still starts; the
// pairing shows as an error connection that can be revoked and re-paired.
func TestMigrateV1MissingKeyIsIsolated(t *testing.T) {
	dir := t.TempDir()
	v1, _ := json.Marshal(localIdentity{DataKey: strings.Repeat("ab", 32), DeviceID: "dev_1", RemoteURL: "https://saas.example"})
	_ = os.WriteFile(filepath.Join(dir, "identity.json"), v1, 0o600)
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("startup blocked: %v", err)
	}
	core, err := newCore(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	runtime.local, runtime.core, runtime.dir = id, core, dir
	defer func() { runtime.core = nil }()
	c := conns(t)[0].(map[string]any)
	if c["state"] != "error" || !strings.Contains(c["last_error"].(string), "missing") {
		t.Fatalf("connection %v", c)
	}
	must(t, map[string]any{"action": "connection_revoke", "connection_id": c["id"]})
	if len(conns(t)) != 0 || len(onDisk(t, dir).Bindings) != 0 {
		t.Fatal("broken binding not removable")
	}
}

// Review2 #3: a permission/I/O error reading the v1 key is not "missing":
// nothing is migrated or rewritten, and the next start migrates normally.
func TestMigrateV1KeyReadErrorKeepsV1(t *testing.T) {
	dir := t.TempDir()
	dk := strings.Repeat("ab", 32)
	v1, _ := json.Marshal(localIdentity{DataKey: dk, DeviceID: "dev_1", RemoteURL: "https://saas.example"})
	f := filepath.Join(dir, "identity.json")
	_ = os.WriteFile(f, v1, 0o600)
	c, _ := durable.NewCipher(dk)
	key, _ := c.Seal(fakeIdentity("https://gw.example", "acme", "dev_1"))
	_ = os.WriteFile(filepath.Join(dir, deviceKeyFile), key, 0o600)
	readFile = func(string) ([]byte, error) {
		return nil, &os.PathError{Op: "open", Path: "device_key.enc", Err: syscall.EACCES}
	}
	_, err := loadIdentity(dir)
	readFile = os.ReadFile
	if err == nil || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("want EACCES error, got %v", err)
	}
	if b, _ := os.ReadFile(f); string(b) != string(v1) {
		t.Fatal("identity.json was changed")
	}
	if _, err := os.Stat(f + ".v1.bak"); err == nil {
		t.Fatal("backup written although nothing migrated")
	}
	if _, err := os.Stat(filepath.Join(dir, deviceKeyFile)); err != nil {
		t.Fatal("v1 key touched")
	}
	// next start (access fixed): normal migration, a working binding
	id, err := loadIdentity(dir)
	if err != nil || len(id.Bindings) != 1 {
		t.Fatalf("retry: %v %+v", err, id)
	}
	if _, err := loadBinding(dir, id, id.Bindings[0]); err != nil {
		t.Fatalf("migrated key unusable: %v", err)
	}
}

// dataDirJSON encodes a start request; Windows paths contain backslashes that
// must be JSON-escaped.
func dataDirJSON(dir string) string {
	b, _ := json.Marshal(map[string]string{"data_dir": dir})
	return string(b)
}
