package connector

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"localsubproxy/internal/audit"
	"localsubproxy/internal/durable"
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
}

func viewAdapter(a AdapterConfig) AdapterView {
	return AdapterView{ID: a.ID, Type: a.Type, Name: a.Name, Provider: a.Provider, BaseURL: a.BaseURL, CodexBin: a.CodexBin, Models: append([]string(nil), a.Models...), ContextWindow: a.ContextWindow, MaxOutputTokens: a.MaxOutputTokens, ReasoningEffort: a.ReasoningEffort, IsDefault: a.IsDefault, Enabled: a.Enabled, HasAPIKey: a.APIKey != "", SkipModelProbe: a.SkipModelProbe, Capabilities: capsOf(a), Concurrency: adapterSlots(a)}
}

func (x *Connector) Status() map[string]any {
	x.mu.Lock()
	running, remoteOK, gatewayOK, lastErr := len(x.running), x.remoteOK, x.gatewayOK, x.lastErr
	x.mu.Unlock()
	deviceID, gatewayURL, transport, e2ee := "", "", "offline", false
	if id := x.cfg.Identity; id != nil {
		deviceID, gatewayURL, e2ee = id.DeviceID, id.GatewayURL, id.E2EEKey != ""
		transport = "wss"
		if strings.HasPrefix(gatewayURL, "http://") {
			transport = "ws"
		}
	}
	return map[string]any{"protocol": "lsp2", "last_error": lastErr, "device_id": deviceID, "device_name": localDeviceName(), "server_url": gatewayURL,
		"running_tasks": running, "max_concurrency": x.cfg.MaxConcurrency, "transport": transport, "e2ee": e2ee,
		"connected": remoteOK && gatewayOK, "remote_connected": remoteOK, "gateway_connected": gatewayOK}
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
	return durable.Write(x.cfg.AdapterFile, b)
}

func (x *Connector) activeAdapterLocked(id string) bool {
	for _, a := range x.running {
		if a == id {
			return true
		}
	}
	return false
}

func (x *Connector) CreateAdapter(a AdapterConfig) (AdapterView, error) {
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
	x.mu.Lock()
	old, exists := x.adapters[a.ID]
	if !exists {
		x.mu.Unlock()
		return AdapterView{}, errors.New("adapter not found")
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
	x.mu.Lock()
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
	err := x.saveAdaptersLocked()
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
