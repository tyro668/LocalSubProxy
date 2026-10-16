package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/audit"
	"github.com/tyro668/LocalSubProxy/internal/durable"
	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// AdapterView is the local UI representation. It never contains an API key.
type AdapterView struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Name            string   `json:"name"`
	Provider        string   `json:"provider,omitempty"`
	BaseURL         string   `json:"base_url,omitempty"`
	CodexBin        string   `json:"codex_bin,omitempty"`
	Models          []string `json:"models"`
	ContextWindow   int      `json:"context_window,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	IsDefault       bool     `json:"is_default"`
	Enabled         bool     `json:"enabled"`
	HasAPIKey       bool     `json:"has_api_key"`
	SkipModelProbe  bool     `json:"skip_model_probe"`
	Capabilities    []string `json:"capabilities"`
	Concurrency     int      `json:"concurrency"`
	Health          string   `json:"health"`
	AuthState       string   `json:"auth_state"`
	// edit-side values (what is stored); Capabilities/Concurrency above are
	// the computed effective values for display only.
	ConfiguredCapabilities []string `json:"configured_capabilities"`
	ConfiguredConcurrency  int      `json:"configured_concurrency"`
}

func viewAdapter(a AdapterConfig) AdapterView {
	return AdapterView{ID: a.ID, Type: a.Type, Name: a.Name, Provider: a.Provider, BaseURL: a.BaseURL, CodexBin: a.CodexBin, Models: append([]string(nil), a.Models...), ContextWindow: a.ContextWindow, MaxOutputTokens: a.MaxOutputTokens, ReasoningEffort: a.ReasoningEffort, IsDefault: a.IsDefault, Enabled: a.Enabled, HasAPIKey: a.APIKey != "", SkipModelProbe: a.SkipModelProbe, Capabilities: capsOf(a), Concurrency: adapterSlots(a), ConfiguredCapabilities: append([]string(nil), a.Capabilities...), ConfiguredConcurrency: a.Concurrency}
}

// Status is the device-wide status. connections[] lists every paired SaaS;
// the flat connection fields mirror the primary (first) connection for
// clients that predate multi-SaaS support (docs/desktop-status-contract.md).
func (x *Connector) Status() map[string]any {
	x.mu.Lock()
	running, recovery := len(x.running), x.recovery
	conns := make([]map[string]any, 0, len(x.order))
	online := 0
	for _, id := range x.order {
		c := x.linkStatusLocked(x.links[id])
		if c["connected"] == true {
			online++
		}
		conns = append(conns, c)
	}
	x.mu.Unlock()
	st := map[string]any{"protocol": "lmbp", "last_error": "", "device_id": "", "device_name": localDeviceName(), "server_url": "",
		"issuer": "", "saas_name": "", "tenant": "", "audit": x.cfg.Audit != nil,
		"running_tasks": running, "max_concurrency": x.cfg.MaxConcurrency, "transport": "offline", "e2ee": false,
		"connected": false, "remote_connected": false, "gateway_connected": false,
		"connections": conns, "connections_total": len(conns), "connections_online": online}
	if len(conns) > 0 {
		p := conns[0]
		for _, k := range []string{"last_error", "device_id", "server_url", "issuer", "saas_name", "tenant", "transport", "e2ee", "connected", "remote_connected", "gateway_connected"} {
			st[k] = p[k]
		}
	}
	if recovery != nil {
		st["adapter_recovery"] = recovery
	}
	return st
}

func (x *Connector) ListAdapters() []AdapterView {
	x.mu.Lock()
	defer x.mu.Unlock()
	list := make([]AdapterView, 0, len(x.adapters))
	for _, a := range x.adapters {
		view := viewAdapter(a)
		view.Health = x.health[a.ID].Health
		view.AuthState = x.health[a.ID].AuthState
		list = append(list, view)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	hasDefault := false
	for _, a := range list {
		hasDefault = hasDefault || a.IsDefault
	}
	if !hasDefault && len(list) > 0 {
		list[0].IsDefault = true
	}
	return list
}

func (x *Connector) saveAdaptersLocked() error {
	x.cfgVer++ // memory changed (saved or rolled back after this)
	if x.cfg.AdapterFile == "" {
		return errors.New("adapter file not configured")
	}
	list := make([]AdapterConfig, 0, len(x.adapters))
	for _, a := range x.adapters {
		list = append(list, a)
	}
	b, err := x.cipher.Seal(list)
	if err != nil {
		return err
	}
	if x.adaptersBlocked != nil {
		return x.adaptersBlocked
	}
	err = writeAdapterFile(x.cfg.AdapterFile, b)
	if err == nil {
		x.lastSaved = b
		return nil
	}
	if !durable.Committed(err) {
		return err // old file intact; the caller rolls back memory
	}
	// Renamed but the directory sync failed: read it back.
	if got, rerr := readAdapterFile(x.cfg.AdapterFile); rerr == nil && bytes.Equal(got, b) {
		x.lastSaved, x.storageWarning = b, "adapters.enc saved but its directory could not be synced: "+err.Error()
		return nil // committed: memory keeps the change
	}
	// Unverifiable: put the previous content back so the caller's
	// rollback matches the disk; if even that fails, refuse further
	// adapter changes until restart (memory and disk may differ).
	restore := func() error { return writeAdapterFile(x.cfg.AdapterFile, x.lastSaved) }
	if x.lastSaved == nil { // there was no file before
		restore = func() error { return os.Remove(x.cfg.AdapterFile) }
	}
	if rerr := restore(); rerr == nil {
		return err
	}
	x.adaptersBlocked = fmt.Errorf("adapters.enc state is uncertain after a failed save (%v); restart LocalSubProxy before changing adapters", err)
	return x.adaptersBlocked
}

// AdapterStorageWarning is set when adapters.enc was committed but its
// directory could not be synced.
func (x *Connector) AdapterStorageWarning() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.storageWarning
}

// Injected in tests (adapters.enc commit phases).
var (
	writeAdapterFile = durable.Write
	readAdapterFile  = os.ReadFile
)

func (x *Connector) activeAdapterLocked(id string) bool {
	for _, r := range x.running {
		if r.adapter == id {
			return true
		}
	}
	return false
}

var errCoreStopped = errors.New("LocalSubProxy core stopped; retry")

// beginMutLocked admits an adapter change; Shutdown waits for it, and a
// change on a stopped core never touches adapters.enc (a newer core may own
// it by now).
func (x *Connector) beginMutLocked() error {
	if x.closed {
		return errCoreStopped
	}
	x.muts.Add(1)
	return nil
}

// mutHook runs in UpdateAdapter after validation, before re-locking (tests).
var mutHook func()

func (x *Connector) CreateAdapter(a AdapterConfig) (AdapterView, error) {
	x.mu.Lock()
	if err := x.beginMutLocked(); err != nil {
		x.mu.Unlock()
		return AdapterView{}, err
	}
	x.mu.Unlock()
	defer x.muts.Done()
	if err := validateAdapter(&a); err != nil {
		return AdapterView{}, err
	}
	x.mu.Lock()
	if _, exists := x.adapters[a.ID]; exists {
		x.mu.Unlock()
		return AdapterView{}, errors.New("adapter already exists")
	}
	if len(x.adapters) == 0 {
		a.IsDefault = true
	}
	previousDefaults := map[string]bool{}
	if a.IsDefault {
		for id, existing := range x.adapters {
			previousDefaults[id] = existing.IsDefault
			existing.IsDefault = false
			x.adapters[id] = existing
		}
	}
	x.adapters[a.ID] = a
	err := x.saveAdaptersLocked()
	if err != nil {
		delete(x.adapters, a.ID)
		for id, isDefault := range previousDefaults {
			existing := x.adapters[id]
			existing.IsDefault = isDefault
			x.adapters[id] = existing
		}
	}
	x.mu.Unlock()
	if err != nil {
		return AdapterView{}, err
	}
	go x.refreshHeartbeat()
	return viewAdapter(a), nil
}

func (x *Connector) UpdateAdapter(a AdapterConfig) (AdapterView, error) {
	return x.updateAdapter(a.ID, func(AdapterConfig) (AdapterConfig, error) { return a, nil })
}

// viewOnlyKeys are AdapterView fields that are derived, never configuration.
var viewOnlyKeys = []string{"has_api_key", "health", "auth_state", "configured_capabilities", "configured_concurrency"}

// MergeAdapterPatch applies a partial edit to old: keys absent from patch keep
// their stored value, view-only keys are ignored, and capabilities computed
// for display (e.g. param markers) are never persisted as configuration.
func MergeAdapterPatch(old AdapterConfig, patch json.RawMessage) (AdapterConfig, error) {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(patch, &p); err != nil {
		return AdapterConfig{}, err
	}
	for _, k := range viewOnlyKeys {
		delete(p, k)
	}
	if raw, ok := p["capabilities"]; ok {
		var caps []string
		if err := json.Unmarshal(raw, &caps); err != nil {
			return AdapterConfig{}, err
		}
		if !sameStrings(caps, capsOf(old)) { // unchanged effective view: keep stored config
			kept := []string{}
			for _, c := range caps {
				if c == lmbp.ModelCapTools || c == lmbp.ModelCapVision || c == lmbp.ModelCapAudio {
					kept = append(kept, c)
				}
			}
			old.Capabilities = kept
		}
		delete(p, "capabilities")
	}
	if raw, ok := p["concurrency"]; ok {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return AdapterConfig{}, err
		}
		if n != adapterSlots(old) {
			old.Concurrency = n
		}
		delete(p, "concurrency")
	}
	base, err := json.Marshal(old)
	if err != nil {
		return AdapterConfig{}, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return AdapterConfig{}, err
	}
	for k, v := range p {
		m[k] = v
	}
	merged, _ := json.Marshal(m)
	var out AdapterConfig
	if err := json.Unmarshal(merged, &out); err != nil {
		return AdapterConfig{}, err
	}
	out.ID = old.ID
	return out, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// PatchAdapter merges a partial edit onto the stored adapter atomically.
func (x *Connector) PatchAdapter(id string, patch json.RawMessage) (AdapterView, error) {
	return x.updateAdapter(id, func(old AdapterConfig) (AdapterConfig, error) { return MergeAdapterPatch(old, patch) })
}

func (x *Connector) updateAdapter(id string, build func(AdapterConfig) (AdapterConfig, error)) (AdapterView, error) {
	x.mu.Lock()
	if err := x.beginMutLocked(); err != nil {
		x.mu.Unlock()
		return AdapterView{}, err
	}
	defer x.muts.Done()
	ver := x.cfgVer
	old, exists := x.adapters[id]
	if !exists {
		x.mu.Unlock()
		return AdapterView{}, errors.New("adapter not found")
	}
	a, err := build(old)
	if err != nil {
		x.mu.Unlock()
		return AdapterView{}, err
	}
	if x.activeAdapterLocked(a.ID) {
		x.mu.Unlock()
		return AdapterView{}, errors.New("adapter has an active task")
	}
	if a.APIKey == "" && sameCredentialEndpoint(a, old) {
		a.APIKey = old.APIKey
	}
	if old.IsDefault && !a.IsDefault {
		a.IsDefault = true
	}
	x.mu.Unlock()
	if err := validateAdapter(&a); err != nil {
		return AdapterView{}, err
	}
	if mutHook != nil {
		mutHook()
	}
	x.mu.Lock()
	if x.cfgVer != ver {
		// deleted or edited while validating: never write back a stale copy
		x.mu.Unlock()
		return AdapterView{}, errors.New("adapter list changed meanwhile; reload and retry")
	}
	if x.activeAdapterLocked(a.ID) {
		x.mu.Unlock()
		return AdapterView{}, errors.New("adapter has an active task")
	}
	previousDefaults := map[string]bool{}
	if a.IsDefault {
		for id, existing := range x.adapters {
			previousDefaults[id] = existing.IsDefault
			if id != a.ID {
				existing.IsDefault = false
				x.adapters[id] = existing
			}
		}
	}
	x.adapters[a.ID] = a
	err = x.saveAdaptersLocked()
	if err != nil {
		x.adapters[a.ID] = old
		for id, isDefault := range previousDefaults {
			existing := x.adapters[id]
			existing.IsDefault = isDefault
			x.adapters[id] = existing
		}
	}
	x.mu.Unlock()
	if err != nil {
		return AdapterView{}, err
	}
	go x.refreshHeartbeat()
	return viewAdapter(a), nil
}

func (x *Connector) DeleteAdapter(id string) error {
	x.mu.Lock()
	if err := x.beginMutLocked(); err != nil {
		x.mu.Unlock()
		return err
	}
	defer x.muts.Done()
	old, exists := x.adapters[id]
	if !exists {
		x.mu.Unlock()
		return errors.New("adapter not found")
	}
	if x.activeAdapterLocked(id) {
		x.mu.Unlock()
		return errors.New("adapter has an active task")
	}
	delete(x.adapters, id)
	var nextDefaultID string
	if old.IsDefault {
		for candidate := range x.adapters {
			if nextDefaultID == "" || candidate < nextDefaultID {
				nextDefaultID = candidate
			}
		}
		if nextDefaultID != "" {
			next := x.adapters[nextDefaultID]
			next.IsDefault = true
			x.adapters[nextDefaultID] = next
		}
	}
	err := x.saveAdaptersLocked()
	if err != nil {
		x.adapters[id] = old
		if nextDefaultID != "" {
			next := x.adapters[nextDefaultID]
			next.IsDefault = false
			x.adapters[nextDefaultID] = next
		}
	}
	x.mu.Unlock()
	if err == nil {
		go x.refreshHeartbeat()
	}
	return err
}

// TestAdapter checks a draft profile without changing the encrypted model list.
func (x *Connector) TestAdapter(ctx context.Context, a AdapterConfig) (AdapterHealth, error) {
	x.mu.Lock()
	if old, ok := x.adapters[a.ID]; ok && a.APIKey == "" && sameCredentialEndpoint(a, old) {
		a.APIKey = old.APIKey
	}
	x.mu.Unlock()
	if err := validateAdapter(&a); err != nil {
		return AdapterHealth{}, err
	}
	a.Enabled = true
	a.SkipModelProbe = false
	return x.probeAdapter(ctx, a), nil
}

func sameCredentialEndpoint(a, old AdapterConfig) bool {
	return a.Type == old.Type && a.BaseURL == old.BaseURL
}

func (x *Connector) refreshHeartbeat() {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	x.probeAll(ctx)
	cancel()
	x.notifyModels()
}

var errNoAudit = errors.New("audit log not configured")

// AuditList returns recent per-request audit rows (no prompt content).
func (x *Connector) AuditList(ctx context.Context, f audit.Filter) ([]audit.Record, error) {
	if x.cfg.Audit == nil {
		return nil, errNoAudit
	}
	return x.cfg.Audit.List(ctx, f)
}

// UsageStats aggregates token usage by saas, model, day, hour, status or connection.
func (x *Connector) UsageStats(ctx context.Context, f audit.Filter, groupBy []string, local bool) ([]audit.Bucket, error) {
	if x.cfg.Audit == nil {
		return nil, errNoAudit
	}
	return x.cfg.Audit.Stats(ctx, f, groupBy, local)
}
