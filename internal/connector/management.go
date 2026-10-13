package connector

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"localsubproxy/internal/durable"
	"localsubproxy/internal/protocol"
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
	Health          string   `json:"health"`
	AuthState       string   `json:"auth_state"`
}

func viewAdapter(a AdapterConfig) AdapterView {
	return AdapterView{ID: a.ID, Type: a.Type, Name: a.Name, Provider: a.Provider, BaseURL: a.BaseURL, CodexBin: a.CodexBin, Models: append([]string(nil), a.Models...), ContextWindow: a.ContextWindow, MaxOutputTokens: a.MaxOutputTokens, ReasoningEffort: a.ReasoningEffort, IsDefault: a.IsDefault, Enabled: a.Enabled, HasAPIKey: a.APIKey != "", SkipModelProbe: a.SkipModelProbe}
}

func (x *Connector) Status() map[string]any {
	x.mu.Lock()
	running, journalBytes, connected := len(x.running), x.journalBytes, x.online
	remoteConnected, gatewayConnected := x.remoteOK, x.heartbeatOK && !x.pollFailed
	x.mu.Unlock()
	transport := "offline"
	protocolName, deviceID, serverURL, v2err := "v1", x.cfg.DeviceID, x.cfg.ServerURL, ""
	if x.cfg.V2 != nil {
		protocolName, deviceID, serverURL, transport = "v2", x.cfg.V2.DeviceID, x.cfg.V2.GatewayURL, "wss"
		if strings.HasPrefix(serverURL, "http://") {
			transport = "ws"
		}
		x.v2.mu.Lock()
		v2err = x.v2.lastErr
		x.v2.mu.Unlock()
	} else if x.cfg.ServerURL != "" {
		transport = "http"
		if strings.HasPrefix(x.cfg.ServerURL, "https://") {
			transport = "https"
		}
	}
	return map[string]any{"protocol": protocolName, "last_error": v2err, "device_id": deviceID, "device_name": localDeviceName(), "server_url": serverURL, "running_tasks": running, "journal_bytes": journalBytes, "transport": transport, "connected": connected, "remote_connected": remoteConnected, "gateway_connected": gatewayConnected}
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
	if x.v2AdapterActive(id) {
		return true
	}
	for _, t := range x.j.Tasks {
		if t.Command.Request.Target.AdapterID == id && !isTerminal(t.State) {
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
func (x *Connector) TestAdapter(ctx context.Context, a AdapterConfig) (protocol.AdapterHealth, error) {
	x.mu.Lock()
	if old, ok := x.adapters[a.ID]; ok && a.APIKey == "" && sameCredentialEndpoint(a, old) {
		a.APIKey = old.APIKey
	}
	x.mu.Unlock()
	if err := validateAdapter(&a); err != nil {
		return protocol.AdapterHealth{}, err
	}
	a.Enabled = true
	a.SkipModelProbe = false
	return x.probeAdapter(ctx, a), nil
}

func sameCredentialEndpoint(a, old AdapterConfig) bool {
	return a.Type == old.Type && a.BaseURL == old.BaseURL
}

func (x *Connector) refreshHeartbeat() {
	if x.cfg.V2 != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		x.probeAll(ctx)
		cancel()
		x.notifyV2Models()
		return
	}
	if x.cfg.ServerURL == "" {
		return
	}
	for attempt := 0; attempt < 5; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		err := x.heartbeat(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(time.Duration(1<<attempt) * 250 * time.Millisecond)
	}
}
