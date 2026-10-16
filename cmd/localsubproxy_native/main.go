package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/tyro668/LocalSubProxy/internal/audit"
	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/durable"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
)

// localIdentity is identity.json: the per-install data key plus display
// fields. The device private keys live encrypted in device_key.enc.
type localIdentity struct {
	Version   int       `json:"version,omitempty"`
	DataKey   string    `json:"data_key"`
	RemoteURL string    `json:"remote_url,omitempty"` // v1 display field, kept
	DeviceID  string    `json:"device_id,omitempty"`  // v1 only; migrated to Bindings
	Bindings  []binding `json:"bindings,omitempty"`
	// PendingCleanup lists key files (relative to the data dir) of revoked
	// connections that still have to be deleted; retried at every start.
	PendingCleanup []string `json:"pending_cleanup,omitempty"`
	// JournalKey is the upstream app's name for the same key (dda2be5 sealed
	// adapters.enc with it). It is kept on disk so a downgrade still works,
	// and tried as a legacy key when it differs from DataKey.
	JournalKey string `json:"journal_key,omitempty"`
}

// loadIdentity reads identity.json. An upstream file (journal_key only)
// adopts that key as the data key; a new key is generated only when the
// file has no key at all, and existing fields are never discarded.
func loadIdentity(dir string) (localIdentity, error) {
	var id localIdentity
	f := filepath.Join(dir, "identity.json")
	if err := durable.ReadJSON(f, &id); err != nil {
		return id, err
	}
	switch {
	case id.DataKey != "":
		return id, migrateV1(dir, &id)
	case id.JournalKey != "":
		id.DataKey = id.JournalKey
	default:
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return id, err
		}
		id.DataKey = hex.EncodeToString(key)
	}
	if err := durable.WriteJSON(f, id); err != nil {
		return id, err
	}
	return id, migrateV1(dir, &id)
}

func (id localIdentity) legacyKeys() []string {
	if id.JournalKey != "" && id.JournalKey != id.DataKey {
		return []string{id.JournalKey}
	}
	return nil
}

const deviceKeyFile = "device_key.enc"

// loadDevice decrypts the lmbp identity (Ed25519 + X25519 private keys and
// the pinned gateway key) with the per-install data key.
func loadDevice(dir string, id localIdentity) (*client.Identity, error) {
	if id.DeviceID == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, deviceKeyFile))
	if err != nil {
		return nil, err
	}
	c, err := durable.NewCipher(id.DataKey)
	if err != nil {
		return nil, err
	}
	var v client.Identity
	if err := c.Open(b, &v); err != nil || !v.Valid() {
		return nil, errors.New("device key file is corrupt or encrypted with another key; pair again")
	}
	return &v, nil
}

func saveDevice(dir string, id localIdentity, v client.Identity) error {
	c, err := durable.NewCipher(id.DataKey)
	if err != nil {
		return err
	}
	b, err := c.Seal(v)
	if err != nil {
		return err
	}
	return durable.Write(filepath.Join(dir, deviceKeyFile), b)
}

type pairSession struct {
	id     string
	req    *client.PairRequest // verified, awaiting the user's consent decision
	p      *client.Pairing
	cancel context.CancelFunc
	state  string // consent | pending | paired | failed | denied | cancelled
	err    string
	policy connector.Policy
	conn   string // binding id once paired
}

var runtime struct {
	sync.Mutex
	core        *connector.Connector
	cancel      context.CancelFunc
	dir         string
	local       localIdentity
	broken      map[string]string // binding id → why its key could not be loaded
	leftovers   []string          // orphan key dirs that could not be deleted
	cleanErrs   []string          // why pending_cleanup items are still there
	storageWarn string            // last write committed but not synced
	uncertain   bool              // identity.json may differ from runtime.local
	keepClosed  map[string]bool   // links closed by an uncertain revoke
	landed      map[string]bool   // revokes confirmed by reconcile
	login       *connector.CodexLogin
	loginGen    uint64     // bumped by codex login start/cancel/logout and stop; a start that returns under an older generation is stale
	audit       *audit.Log // per-connection request log (audit.db)
	auditStop   context.CancelFunc
	// lmbp pairings (one SaaS each); lastPair is the most recent
	pairs    map[string]*pairSession
	lastPair string
}

func result(data any, err error) string {
	payload := map[string]any{"ok": err == nil}
	if err != nil {
		payload["error"] = err.Error()
	} else if data != nil {
		payload["data"] = data
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

// newCore builds the shared adapter pool and one link per binding. A
// binding whose key cannot be opened is reported, not fatal.
func newCore(dir string, id localIdentity) (*connector.Connector, error) {
	core, err := connector.New(connector.Config{DataKey: id.DataKey, LegacyDataKeys: id.legacyKeys(), AdapterFile: filepath.Join(dir, "adapters.enc"), Audit: runtime.audit})
	if err != nil {
		return nil, err
	}
	runtime.broken = map[string]string{}
	for _, b := range id.Bindings {
		if err := addLink(core, dir, id, b); err != nil {
			runtime.broken[b.ID] = err.Error()
		}
	}
	return core, nil
}

func addLink(core *connector.Connector, dir string, id localIdentity, b binding) error {
	v, err := loadBinding(dir, id, b)
	if err != nil {
		return err
	}
	p := b.Policy
	// Probe the paired SaaS origin from the device identity; b.RemoteURL may
	// be a display-only homepage that never serves /.well-known/localsubproxy.
	return core.AddLink(connector.LinkConfig{ID: b.ID, Identity: *v, RemoteURL: v.RemoteURL, Policy: &p, Paused: b.Paused})
}

func pairedLocked() bool { return len(runtime.local.Bindings) > 0 }

// persistLocked writes a candidate identity.json. Every change is
// persisted first and only then applied to runtime.local and the running
// core, so a failed write leaves both unchanged (see commit order notes in
// docs/desktop-status-contract.md).
//
// Outcomes: nil = committed. errUncertain = the file may or may not hold
// cand (the rename happened but the directory sync failed and reading it
// back did not settle it): callers must not do destructive cleanup and must
// not restore state the file may already have dropped. Any other error =
// not committed (the old file is intact).
//
// After a directory-sync failure the file is read back: holding cand it is
// treated as committed (storage_warning in status), holding the previous
// state it is treated as not committed.
func persistLocked(cand localIdentity) error {
	f := filepath.Join(runtime.dir, "identity.json")
	err := writeJSON(f, cand)
	if err == nil || !durable.Committed(err) {
		return err
	}
	want, _ := json.Marshal(cand)
	prev, _ := json.Marshal(runtime.local)
	got, rerr := readFile(f)
	switch {
	case rerr == nil && bytes.Equal(got, want):
		runtime.storageWarn = "identity.json was saved but its directory could not be synced: " + err.Error()
		return nil
	case rerr == nil && bytes.Equal(got, prev):
		return err
	}
	runtime.uncertain = true
	return fmt.Errorf("%w: %v", errUncertain, err)
}

// reconcileLocked runs before every config change after an uncertain
// write: it reloads identity.json from disk and makes the in-memory
// bindings match it, so a later change can never write back a binding the
// disk already dropped. While the disk cannot be read, changes are refused.
// Links left closing by an uncertain revoke stay closed.
func reconcileLocked() error {
	if !runtime.uncertain {
		return nil
	}
	raw, err := readFile(filepath.Join(runtime.dir, "identity.json"))
	var disk localIdentity
	if err == nil {
		err = json.Unmarshal(raw, &disk)
	}
	if err != nil {
		return fmt.Errorf("identity.json could not be verified after an interrupted save (%v); changes are blocked until it can be read — restart LocalSubProxy", err)
	}
	for _, b := range runtime.local.Bindings {
		if disk.find(b.ID) >= 0 {
			continue
		}
		// the uncertain revoke did land: finish it (whatever is loaded,
		// even a link whose state change failed, is torn down)
		if runtime.core.HasLink(b.ID) {
			runtime.core.FinishRemove(b.ID)
		}
		delete(runtime.broken, b.ID)
		delete(runtime.keepClosed, b.ID)
		if runtime.landed == nil {
			runtime.landed = map[string]bool{}
		}
		runtime.landed[b.ID] = true
	}
	// Bring the running core in line with every remaining difference:
	// bindings the disk added (an uncertain pairing that landed) and
	// policy / pause changes the disk holds.
	for _, b := range disk.Bindings {
		i := runtime.local.find(b.ID)
		if i < 0 {
			if err := addLink(runtime.core, runtime.dir, disk, b); err != nil {
				if runtime.broken == nil {
					runtime.broken = map[string]string{}
				}
				runtime.broken[b.ID] = err.Error()
			}
			continue
		}
		if !runtime.core.HasLink(b.ID) || runtime.keepClosed[b.ID] {
			continue // not loaded / stays closed
		}
		old := runtime.local.Bindings[i]
		// A loaded link always gets the saved state, even if it was marked
		// broken earlier; if that fails it stops taking tasks (never keeps
		// serving under a state the disk no longer holds).
		if !slices.Equal(old.Policy.Allow, b.Policy.Allow) || old.Policy.MaxConcurrency != b.Policy.MaxConcurrency {
			if err := runtime.core.SetPolicy(b.ID, b.Policy); err != nil {
				runtime.broken[b.ID] = "apply saved policy: " + err.Error()
				runtime.core.Fence(b.ID)
			}
		}
		if old.Paused != b.Paused {
			if err := setPaused(runtime.core, b.ID, b.Paused); err != nil {
				runtime.broken[b.ID] = "apply saved pause state: " + err.Error()
				runtime.core.Fence(b.ID)
			}
		}
	}
	runtime.local, runtime.uncertain = disk, false
	retryCleanupLocked()
	return nil
}

// setPaused is injected in tests to make applying a pause fail.
var setPaused = (*connector.Connector).SetPaused

var errUncertain = errors.New("identity.json state is uncertain (saved file could not be verified); nothing was deleted, restart LocalSubProxy and check the connection")

func (id localIdentity) clone() localIdentity {
	c := id
	c.Bindings = make([]binding, len(id.Bindings))
	for i, b := range id.Bindings {
		b.Policy.Allow = append([]string{}, b.Policy.Allow...)
		c.Bindings[i] = b
	}
	c.PendingCleanup = append([]string(nil), id.PendingCleanup...)
	return c
}

func (id localIdentity) without(bid string) localIdentity {
	c := id.clone()
	if i := c.find(bid); i >= 0 {
		c.Bindings = append(c.Bindings[:i:i], c.Bindings[i+1:]...)
	}
	return c
}

// retryCleanupLocked deletes pending_cleanup items and persists what is
// left. If that write fails the list on disk is longer than needed, which
// is harmless: a missing file counts as deleted.
func retryCleanupLocked() {
	if len(runtime.local.PendingCleanup) == 0 {
		runtime.cleanErrs = nil
		return
	}
	left, errs := runCleanup(runtime.dir, runtime.local.PendingCleanup)
	runtime.cleanErrs = errs
	if len(left) == len(runtime.local.PendingCleanup) {
		return
	}
	cand := runtime.local.clone()
	cand.PendingCleanup = left
	if persistLocked(cand) == nil {
		runtime.local = cand
	}
}

func appendNew(xs []string, add ...string) []string {
	for _, a := range add {
		if !slices.Contains(xs, a) {
			xs = append(xs, a)
		}
	}
	return xs
}

func noteCleanupLocked(errs []string) {
	runtime.leftovers = append(runtime.leftovers, errs...)
}

func statusLocked() map[string]any {
	s := runtime.core.Status()
	conns, _ := s["connections"].([]map[string]any)
	byID := map[string]map[string]any{}
	for _, c := range conns {
		byID[c["id"].(string)] = c
	}
	out := make([]map[string]any, 0, len(runtime.local.Bindings))
	for _, b := range runtime.local.Bindings {
		c := byID[b.ID]
		if c == nil {
			c = map[string]any{"id": b.ID, "saas_name": b.SaaSName, "issuer": b.Issuer, "tenant": b.Tenant, "device_id": b.DeviceID,
				"state": "error", "connected": false, "remote_connected": false, "gateway_connected": false, "running_tasks": 0,
				"allow": b.Policy.Allow, "paused": b.Paused, "last_error": runtime.broken[b.ID]}
		}
		c["display_url"], c["paired_at"] = b.RemoteURL, b.PairedAt
		out = append(out, c)
	}
	s["connections"], s["connections_total"] = out, len(out)
	s["remote_url"] = runtime.local.RemoteURL
	if len(runtime.local.Bindings) > 0 {
		s["remote_url"] = runtime.local.Bindings[0].RemoteURL
		s["connection_id"] = runtime.local.Bindings[0].ID
	}
	s["paired"] = pairedLocked()
	s["cleanup_errors"] = append(append([]string{}, runtime.cleanErrs...), runtime.leftovers...)
	if runtime.storageWarn != "" {
		s["storage_warning"] = runtime.storageWarn
	}
	if w := runtime.core.AdapterStorageWarning(); w != "" {
		s["adapter_storage_warning"] = w
	}
	s["cleanup_pending"] = append([]string{}, runtime.local.PendingCleanup...)
	if len(runtime.local.PendingCleanup) > 0 || len(runtime.leftovers) > 0 {
		s["warning"] = "some private key files of removed connections could not be deleted yet (retried at every start): " +
			strings.Join(append(append([]string{}, runtime.cleanErrs...), runtime.leftovers...), "; ")
	}
	s["pairing_state"], s["pairing_error"], s["pair_id"] = "idle", "", ""
	if ps := runtime.pairs[runtime.lastPair]; ps != nil {
		s["pairing_state"], s["pairing_error"], s["pair_id"] = ps.state, ps.err, ps.id
	}
	return s
}

//export LSP_Start
func LSP_Start(raw *C.char) *C.char {
	in := ""
	if raw != nil {
		in = C.GoString(raw)
	}
	return C.CString(start(in))
}

func start(raw string) string {
	var input struct {
		DataDir string `json:"data_dir"`
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			return result(nil, err)
		}
	}
	dir := input.DataDir
	if dir == "" {
		userDir, err := os.UserConfigDir()
		if err != nil {
			return result(nil, err)
		}
		dir = filepath.Join(userDir, "localsubproxy")
	}
	runtime.Lock()
	defer runtime.Unlock()
	if runtime.core != nil {
		return result(statusLocked(), nil)
	}
	runtime.pairs, runtime.lastPair = map[string]*pairSession{}, ""
	id, err := loadIdentity(dir)
	if err != nil {
		return result(nil, err)
	}
	openedAudit := runtime.audit == nil // held as temporary until the core starts
	if runtime.audit == nil {
		al, err := audit.Open(filepath.Join(dir, "audit.db"))
		if err != nil {
			return result(nil, err)
		}
		actx, acancel := context.WithCancel(context.Background())
		retired := make(chan struct{})
		go func() { defer close(retired); al.RunRetention(actx, audit.DefaultRetention, time.Hour) }()
		// stop waits for the retention pass to let go of the DB, so a quick
		// restart can reopen audit.db (WAL switch needs the file unlocked)
		runtime.audit, runtime.auditStop = al, func() { acancel(); <-retired }
	}
	runtime.dir, runtime.local = dir, id
	runtime.uncertain, runtime.keepClosed, runtime.landed = false, nil, nil
	runtime.leftovers = cleanupOrphans(dir, id) // e.g. a pairing whose identity.json write failed
	runtime.cleanErrs = nil
	retryCleanupLocked()
	core, err := buildCore(dir, id)
	if err != nil {
		// Review7 #17: release the audit log this start opened, so a later
		// start (possibly with another data_dir) opens its own audit.db.
		if openedAudit && runtime.audit != nil {
			if runtime.auditStop != nil {
				runtime.auditStop()
			}
			_ = runtime.audit.Close()
			runtime.audit, runtime.auditStop = nil, nil
		}
		return result(nil, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime.core, runtime.cancel = core, cancel
	core.Start(ctx)
	return result(statusLocked(), nil)
}

// buildCore is newCore; tests replace it to inject a build failure.
var buildCore = newCore

// pairWaitHook lets tests delay the background wait (scheduling races).
var pairWaitHook = func() {}

// finishPairing runs in the background until the user approves the code on
// the remote service. p is captured at approve time: ps.p may be cleared
// concurrently by cancel/stop/a new pairing and is never read here.
//
// Commit order: key file → identity.json → start the link. A failed write
// removes the new key file; a link that fails to start is rolled back from
// identity.json (or, if that write fails too, kept as a broken binding that
// the user can revoke).
func finishPairing(ctx context.Context, ps *pairSession, p *client.Pairing, remoteURL string) {
	pairWaitHook()
	v, err := p.Wait(ctx)
	runtime.Lock()
	defer runtime.Unlock()
	if runtime.pairs[ps.id] != ps || ps.state != "pending" || ctx.Err() != nil {
		return // cancelled or superseded
	}
	ps.p, ps.cancel = nil, nil
	fail := func(err error) { ps.state, ps.err = "failed", err.Error() }
	if err != nil {
		fail(err)
		if client.IsConsentDenied(err) {
			ps.state = "denied"
		}
		return
	}
	if runtime.core == nil {
		fail(errors.New("local kernel stopped"))
		return
	}
	if err := reconcileLocked(); err != nil {
		fail(err)
		return
	}
	bid, err := newBindingID()
	if err != nil {
		fail(err)
		return
	}
	b := binding{ID: bid, DeviceID: v.DeviceID, Issuer: v.Issuer, Tenant: v.Tenant, SaaSName: v.SaaSName, RemoteURL: remoteURL,
		KeyFile: bindingKeyFile(bid), PairedAt: time.Now().UTC().Format(time.RFC3339), Policy: ps.policy}
	if err := saveBinding(runtime.dir, runtime.local, b, v); err != nil {
		noteCleanupLocked(removeBindingFiles(runtime.dir, b))
		fail(err)
		return
	}
	cand := runtime.local.clone()
	cand.Bindings = append(cand.Bindings, b)
	if err := persistLocked(cand); err != nil {
		if !errors.Is(err, errUncertain) { // not referenced: safe to delete
			noteCleanupLocked(removeBindingFiles(runtime.dir, b))
		} // uncertain: keep the key; orphan cleanup decides at next start
		fail(fmt.Errorf("save identity.json: %w", err))
		return
	}
	runtime.local = cand
	if err := addLink(runtime.core, runtime.dir, runtime.local, b); err != nil {
		back := runtime.local.without(bid)
		if perr := persistLocked(back); perr != nil {
			// not committed or uncertain: the binding may still be on
			// disk, so keep the key and list it (state error) for revoke
			runtime.broken[bid] = err.Error()
		} else {
			runtime.local = back
			noteCleanupLocked(removeBindingFiles(runtime.dir, b))
		}
		fail(err)
		return
	}
	ps.state, ps.err, ps.conn = "paired", "", bid
}

func cancelPairLocked(ps *pairSession) {
	if ps == nil {
		return
	}
	if ps.cancel != nil {
		ps.cancel()
	}
	if ps.state == "pending" || ps.state == "consent" || ps.state == "approving" {
		ps.state = "cancelled"
	}
	ps.req, ps.p, ps.cancel = nil, nil, nil
}

func cancelAllPairingsLocked() {
	for _, ps := range runtime.pairs {
		cancelPairLocked(ps)
	}
}

// pairLocked finds the pairing named by pair_id (default: the latest).
func pairLocked(id string) *pairSession {
	if id == "" {
		id = runtime.lastPair
	}
	return runtime.pairs[id]
}

// bindingLocked resolves connection_id; empty is allowed only with exactly one binding.
func bindingLocked(id string) (int, error) {
	if id == "" {
		if len(runtime.local.Bindings) == 1 {
			return 0, nil
		}
		return -1, errors.New("connection_id required")
	}
	if i := runtime.local.find(id); i >= 0 {
		return i, nil
	}
	return -1, errors.New("unknown connection")
}

func duplicateSaaSLocked(gateway, tenant string) bool {
	for _, b := range runtime.local.Bindings {
		if b.Issuer == gateway && b.Tenant == tenant {
			return true
		}
	}
	return false
}

func availableModels() []string {
	out := []string{}
	for _, a := range runtime.core.ListAdapters() {
		if !a.Enabled {
			continue
		}
		for _, m := range a.Models {
			out = append(out, a.ID+"/"+m)
		}
	}
	return out
}

func pairView(ps *pairSession) map[string]any {
	out := map[string]any{"pair_id": ps.id, "state": ps.state, "error": ps.err}
	if ps.req != nil {
		out["consent"] = ps.req.Consent
	}
	if ps.p != nil {
		out["consent"] = ps.p.Consent
		out["user_code"], out["link_url"], out["expires_at"] = ps.p.UserCode, ps.p.LinkURL, ps.p.ExpiresAt
	}
	if ps.conn != "" {
		out["connection_id"] = ps.conn
	}
	return out
}

//export LSP_Call
func LSP_Call(raw *C.char) *C.char {
	if raw == nil {
		return C.CString(result(nil, errors.New("request required")))
	}
	return C.CString(call(C.GoString(raw)))
}

func call(raw string) string {
	var request struct {
		Action       string                  `json:"action"`
		Adapter      connector.AdapterConfig `json:"adapter"`
		ID           string                  `json:"id"`
		Path         string                  `json:"path"`
		RemoteURL    string                  `json:"remote_url"`
		Link         string                  `json:"link"` // lmbp://pair deep link
		Filter       audit.Filter            `json:"filter"`
		GroupBy      []string                `json:"group_by"`
		LocalTime    bool                    `json:"local_time"`
		Days         int                     `json:"days"`
		ConnectionID string                  `json:"connection_id"` // binding id (status connections[].id)
		PairID       string                  `json:"pair_id"`
		Allow        []string                `json:"allow"` // pair_approve: models this SaaS may use
		MaxConc      int                     `json:"max_concurrency"`
		Policy       *json.RawMessage        `json:"policy"` // connection_policy_set: partial {allow?, max_concurrency?}
	}
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		return result(nil, err)
	}
	if request.Action == "update_adapter" {
		var r struct {
			Adapter json.RawMessage `json:"adapter"`
		}
		_ = json.Unmarshal([]byte(raw), &r)
		if len(r.Adapter) > 0 && request.Adapter.ID != "" {
			runtime.Lock()
			core := runtime.core
			runtime.Unlock()
			if core == nil {
				return result(nil, errors.New("local kernel not running"))
			}
			// partial edit: absent keys keep their stored value
			v, err := core.PatchAdapter(request.Adapter.ID, r.Adapter)
			return result(v, err)
		}
	}
	remoteURL := request.RemoteURL
	// Actions that wait on the network or an external process run without
	// the global lock (the connector has its own), so status polling and
	// other calls are never stuck behind a slow model test.
	switch request.Action {
	case "test_adapter", "codex_models", "create_adapter", "update_adapter":
		runtime.Lock()
		core := runtime.core
		runtime.Unlock()
		if core == nil {
			return result(nil, errors.New("local kernel not running"))
		}
		return unlockedCall(core, request.Action, request.Adapter, request.Path)
	case "codex_login_start", "codex_login_check", "codex_login_cancel", "codex_logout":
		return codexLoginCall(request.Action, request.Path)
	}
	runtime.Lock()
	defer runtime.Unlock()
	if request.Action == "stop" {
		cancelAllPairingsLocked()
		runtime.loginGen++
		if runtime.login != nil {
			runtime.login.Close()
			runtime.login = nil
		}
		if runtime.core != nil {
			runtime.cancel()
			runtime.core.Shutdown()
		}
		runtime.core, runtime.cancel = nil, nil
		// The audit log belongs to this data dir: close it so a later
		// start with another data_dir opens its own audit.db.
		if runtime.audit != nil {
			if runtime.auditStop != nil {
				runtime.auditStop()
			}
			_ = runtime.audit.Close()
			runtime.audit, runtime.auditStop = nil, nil
		}
		return result(nil, nil)
	}
	if runtime.core == nil {
		return result(nil, errors.New("local kernel not running"))
	}
	switch request.Action {
	case "connection_policy_set", "connection_disconnect", "connection_connect", "disconnect", "connection_revoke", "pair_approve", "pair_start", "pair_prepare":
		if err := reconcileLocked(); err != nil {
			return result(nil, err)
		}
	}
	// connection_id narrows audit queries to one binding (device id + gateway)
	if request.ConnectionID != "" && (request.Action == "audit_list" || request.Action == "usage_stats") {
		i, err := bindingLocked(request.ConnectionID)
		if err != nil {
			return result(nil, err)
		}
		b := runtime.local.Bindings[i]
		request.Filter.ConnectionID, request.Filter.Gateway, request.Filter.Tenant = b.DeviceID, b.Issuer, b.Tenant
	}
	switch request.Action {
	case "status":
		return result(statusLocked(), nil)
	case "connections_list":
		return result(map[string]any{"connections": statusLocked()["connections"]}, nil)
	case "audit_list":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rows, err := runtime.core.AuditList(ctx, request.Filter)
		return result(map[string]any{"records": rows}, err)
	case "usage_stats":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b, err := runtime.core.UsageStats(ctx, request.Filter, request.GroupBy, request.LocalTime)
		return result(map[string]any{"buckets": b, "group_by": request.GroupBy}, err)
	case "audit_purge":
		if request.Days < 0 {
			return result(nil, errors.New("days must be >= 0"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		n, err := runtime.audit.Purge(ctx, time.Now().Add(-time.Duration(request.Days)*24*time.Hour))
		return result(map[string]any{"removed": n}, err)
	case "list_adapters":
		return result(map[string]any{"adapters": runtime.core.ListAdapters()}, nil)
	case "adapter_recovery_ack":
		return result(nil, runtime.core.AcknowledgeAdapterRecovery())
	case "delete_adapter":
		return result(nil, runtime.core.DeleteAdapter(request.ID))
	case "pair_start", "pair_prepare":
		// Step 1: resolve the SaaS (remote_url or lmbp://pair link), verify the
		// gateway's signed metadata and return the consent screen data. Nothing
		// is registered at the gateway until pair_approve. Allowed while other
		// SaaS are paired; one pairing is in progress at a time.
		cancelAllPairingsLocked()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		var req *client.PairRequest
		var err error
		if request.Link != "" {
			req, err = client.PrepareLink(ctx, nil, request.Link)
		} else {
			req, err = client.Prepare(ctx, nil, remoteURL)
		}
		cancel()
		if err != nil {
			return result(nil, err)
		}
		if duplicateSaaSLocked(req.Consent.Gateway, req.Consent.Tenant) {
			return result(nil, errors.New("already connected to this SaaS (same gateway and tenant); revoke it first"))
		}
		pid, err := newBindingID()
		if err != nil {
			return result(nil, err)
		}
		pid = "p_" + pid[2:]
		ps := &pairSession{id: pid, req: req, state: "consent"}
		runtime.pairs = map[string]*pairSession{pid: ps} // finished pairings are dropped
		runtime.lastPair = pid
		return result(map[string]any{"pair_id": pid, "state": "consent", "consent": req.Consent, "available_models": availableModels()}, nil)
	case "pair_approve":
		// Step 2a: the user explicitly approved the SaaS shown on the consent
		// screen and chose the models it may use (allow; empty denies all).
		ps := pairLocked(request.PairID)
		if ps == nil || ps.req == nil {
			return result(nil, errors.New("no pairing awaiting consent"))
		}
		pol := connector.Policy{Allow: append([]string{}, request.Allow...), MaxConcurrency: request.MaxConc}
		if pol.Allow == nil {
			pol.Allow = []string{}
		}
		if err := runtime.core.ValidatePolicy(pol); err != nil {
			return result(nil, err)
		}
		req := ps.req
		ps.req, ps.state = nil, "approving" // no second approve/deny meanwhile
		// The network request runs without the global lock (status keeps
		// answering). On failure the request stays retryable: the consent
		// page and pair_id remain valid and pair_approve can be sent again.
		runtime.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		p, err := pairApprove(ctx, req)
		cancel()
		runtime.Lock()
		if ps.state != "approving" { // cancelled or the kernel stopped meanwhile
			return result(nil, errors.New("pairing was cancelled"))
		}
		if err != nil {
			ps.req, ps.state, ps.err = req, "consent", err.Error()
			return result(nil, fmt.Errorf("%w (you can retry)", err))
		}
		wctx, wcancel := context.WithCancel(context.Background())
		remote := req.Consent.Remote // Consent.Remote falls back to the pairing page origin in client.Prepare; stay empty rather than probing a marketing homepage
		ps.p, ps.cancel, ps.state, ps.err, ps.policy = p, wcancel, "pending", "", pol
		go finishPairing(wctx, ps, p, remote)
		return result(map[string]any{"pair_id": ps.id, "user_code": p.UserCode, "link_url": p.LinkURL, "jkt": p.JKT, "expires_at": p.ExpiresAt, "consent": p.Consent}, nil)
	case "pair_deny":
		// Step 2b: the user declined; nothing was sent to the gateway.
		ps := pairLocked(request.PairID)
		if ps == nil || ps.req == nil {
			return result(nil, errors.New("no pairing awaiting consent"))
		}
		err := ps.req.Deny()
		ps.req, ps.state, ps.err = nil, "denied", err.Error()
		return result(map[string]any{"pair_id": ps.id, "state": "denied", "code": "CONSENT_DENIED"}, nil)
	case "pair_status":
		ps := pairLocked(request.PairID)
		if ps == nil {
			return result(map[string]any{"state": "idle", "error": ""}, nil)
		}
		out := pairView(ps)
		if ps.state == "paired" {
			out["status"] = statusLocked()
		}
		return result(out, nil)
	case "pair_cancel":
		cancelPairLocked(pairLocked(request.PairID))
		return result(nil, nil)
	case "connection_policy_set":
		// Partial update: only the fields present in policy change.
		i, err := bindingLocked(request.ConnectionID)
		if err != nil {
			return result(nil, err)
		}
		if request.Policy == nil {
			return result(nil, errors.New("policy required"))
		}
		var patch struct {
			Allow          *[]string `json:"allow"`
			MaxConcurrency *int      `json:"max_concurrency"`
		}
		if err := json.Unmarshal(*request.Policy, &patch); err != nil {
			return result(nil, err)
		}
		cand := runtime.local.clone()
		b := &cand.Bindings[i]
		if patch.Allow != nil {
			b.Policy.Allow = append([]string{}, (*patch.Allow)...)
		}
		if patch.MaxConcurrency != nil {
			b.Policy.MaxConcurrency = *patch.MaxConcurrency
		}
		if b.Policy.Allow == nil {
			b.Policy.Allow = []string{}
		}
		if err := runtime.core.ValidatePolicy(b.Policy); err != nil {
			return result(nil, err)
		}
		if err := persistLocked(cand); err != nil {
			return result(nil, fmt.Errorf("save identity.json: %w", err))
		}
		old := runtime.local
		runtime.local = cand
		// Any loaded link gets the policy, whether or not it is marked
		// broken: saving it and silently skipping the running link would
		// let it keep serving under the old policy.
		if runtime.core.HasLink(b.ID) {
			if err := runtime.core.SetPolicy(b.ID, b.Policy); err != nil {
				if perr := persistLocked(old); perr != nil {
					// runtime.local stays cand = what is on disk; the live
					// link cannot get it: stop it taking tasks
					runtime.core.Fence(b.ID)
					if runtime.broken == nil {
						runtime.broken = map[string]string{}
					}
					runtime.broken[b.ID] = "apply saved policy: " + err.Error()
					return result(nil, fmt.Errorf("%v; rolling back identity.json also failed (%v): the connection stops taking tasks; the saved policy takes effect at the next start", err, perr))
				}
				runtime.local = old
				return result(nil, err)
			}
		}
		return result(statusLocked(), nil)
	case "connection_disconnect", "connection_connect":
		// Pause or resume one SaaS; the pairing and its keys are kept.
		i, err := bindingLocked(request.ConnectionID)
		if err != nil {
			return result(nil, err)
		}
		id := runtime.local.Bindings[i].ID
		if msg, broken := runtime.broken[id]; broken {
			return result(nil, errors.New(msg))
		}
		paused := request.Action == "connection_disconnect"
		if paused && runtime.core.ConnectionRunning(id) > 0 {
			return result(nil, errors.New("tasks are still running on this connection"))
		}
		cand := runtime.local.clone()
		cand.Bindings[i].Paused = paused
		if err := persistLocked(cand); err != nil {
			return result(nil, fmt.Errorf("save identity.json: %w", err))
		}
		old := runtime.local
		runtime.local = cand
		if err := setPaused(runtime.core, id, paused); err != nil {
			if perr := persistLocked(old); perr != nil {
				// runtime.local stays cand (= disk); the running state
				// differs until the next start applies the saved one
				return result(nil, fmt.Errorf("%v; rolling back identity.json also failed (%v): the saved setting takes effect at the next start", err, perr))
			}
			runtime.local = old
			return result(nil, err)
		}
		return result(statusLocked(), nil)
	case "disconnect", "connection_revoke":
		// Forget one SaaS: stop its session and delete its device keys
		// locally (re-pairing generates new ones). Other SaaS keep running.
		// Order: refuse new tasks → persist the removal → stop + wait →
		// delete key files (failures are reported and retried at startup).
		i, err := bindingLocked(request.ConnectionID)
		if err != nil && runtime.landed[request.ConnectionID] {
			// an earlier uncertain revoke turned out to have landed
			delete(runtime.landed, request.ConnectionID)
			st := statusLocked()
			st["removed"] = true
			return result(st, nil)
		}
		if err != nil {
			return result(nil, err)
		}
		b := runtime.local.Bindings[i]
		// "broken" may mean "never loaded" (key unreadable) or "loaded and
		// serving but a state change failed": only the core knows whether a
		// link is actually running, and a running one is always torn down.
		loaded := runtime.core.HasLink(b.ID)
		if loaded {
			if err := runtime.core.BeginRemove(b.ID); err != nil {
				return result(nil, err)
			}
		}
		cand := runtime.local.without(b.ID)
		// The key paths are recorded in the same write that drops the
		// binding, so a crash or delete failure never loses track of them.
		cand.PendingCleanup = appendNew(cand.PendingCleanup, bindingCleanupPaths(b)...)
		if err := persistLocked(cand); err != nil {
			if errors.Is(err, errUncertain) {
				// the file may already drop it: stay closed, keep keys
				if runtime.keepClosed == nil {
					runtime.keepClosed = map[string]bool{}
				}
				runtime.keepClosed[b.ID] = true
			} else if loaded && !runtime.keepClosed[b.ID] {
				runtime.core.AbortRemove(b.ID) // removal not saved: keep serving
			}
			return result(nil, fmt.Errorf("save identity.json: %w", err))
		}
		runtime.local = cand
		delete(runtime.broken, b.ID)
		delete(runtime.keepClosed, b.ID)
		if loaded {
			runtime.core.FinishRemove(b.ID)
		}
		retryCleanupLocked()
		st := statusLocked()
		st["removed"] = true // the connection is gone even if "warning" is set
		return result(st, nil)
	default:
		return result(nil, errors.New("unknown action"))
	}
}

// unlockedCall runs the slow actions outside runtime's lock.
func unlockedCall(core *connector.Connector, action string, a connector.AdapterConfig, path string) string {
	switch action {
	case "create_adapter":
		v, err := core.CreateAdapter(a)
		return result(v, err)
	case "update_adapter":
		v, err := core.UpdateAdapter(a)
		return result(v, err)
	case "test_adapter":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		health, err := core.TestAdapter(ctx, a)
		return result(health, err)
	default: // codex_models
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		models, err := codexModels(ctx, path)
		return result(models, err)
	}
}

// pairApprove is injected in tests (network failure / timing).
var pairApprove = func(ctx context.Context, r *client.PairRequest) (*client.Pairing, error) {
	return r.Approve(ctx, connector.DeviceName())
}

// codexModels is injected in tests (a slow external process).
var codexModels = connector.CodexModels

//export LSP_Free
func LSP_Free(ptr unsafe.Pointer) {
	if ptr != nil {
		C.free(ptr)
	}
}

func main() {}

// codexLoginCall runs Codex login actions without holding the global runtime
// lock across the App Server (start/probe/logout can each take seconds);
// the lock only guards the session pointer. Review7 #9.
func codexLoginCall(action, path string) string {
	runtime.Lock()
	core := runtime.core
	login := runtime.login
	var gen uint64
	if action != "codex_login_check" {
		// start/cancel/logout each open a new generation: a start still
		// running in BeginCodexLogin (runtime.login is nil meanwhile) is
		// thereby superseded and must discard its session. Review8 P2-b.
		runtime.login = nil
		runtime.loginGen++
		gen = runtime.loginGen
	}
	runtime.Unlock()
	if action != "codex_login_check" && login != nil {
		login.Close() // non-blocking
	}
	if core == nil {
		return result(nil, errors.New("local kernel not running"))
	}
	switch action {
	case "codex_login_start":
		next, err := connector.BeginCodexLogin(path)
		if err != nil {
			return result(nil, err)
		}
		runtime.Lock()
		if runtime.core != core || runtime.loginGen != gen || runtime.login != nil {
			runtime.Unlock()
			next.Close()
			return result(nil, errors.New("codex login superseded; start again"))
		}
		runtime.login = next
		runtime.Unlock()
		return result(map[string]string{"auth_url": next.AuthURL}, nil)
	case "codex_login_check":
		if login == nil {
			return result(nil, errors.New("no Codex login in progress"))
		}
		models, complete, err := login.Check()
		if complete || err != nil {
			runtime.Lock()
			if runtime.login == login && (complete || login.Closed()) {
				runtime.login = nil
			}
			runtime.Unlock()
		}
		return result(map[string]any{"complete": complete, "models": models}, err)
	case "codex_logout":
		return result(nil, connector.LogoutCodex(path))
	}
	return result(nil, nil)
}
