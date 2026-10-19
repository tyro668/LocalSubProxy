package connector

import (
	"context"
	"fmt"

	"github.com/tyro668/avora/internal/backend"
)

// BackendInfo describes one scheme type for clients (the desktop renders
// its scheme forms from these).
type BackendInfo struct {
	Type               string   `json:"type"`
	Name               string   `json:"name"`
	Label              string   `json:"label"`
	Short              string   `json:"short"`
	Aliases            []string `json:"aliases"`
	Executable         string   `json:"executable,omitempty"`
	LoginCommand       string   `json:"login_command,omitempty"`
	LoginStatus        bool     `json:"login_status"`
	Cloud              bool     `json:"cloud"`
	NeedsAPIKey        bool     `json:"needs_api_key"`
	DefaultBaseURL     string   `json:"default_base_url,omitempty"`
	Capabilities       []string `json:"capabilities"`
	Parameters         []string `json:"parameters"`
	DefaultConcurrency int      `json:"default_concurrency"`
	MaxConcurrency     int      `json:"max_concurrency,omitempty"`
	Efforts            []string `json:"efforts"`
}

// Backends lists every scheme type this build supports.
func Backends() []BackendInfo {
	out := []BackendInfo{}
	for _, d := range backendRegistry.Definitions() {
		_, login := d.New(backend.Config{Type: d.Type}, backend.Dependencies{}).(backend.LoginChecker)
		out = append(out, BackendInfo{Type: d.Type, Name: d.Name, Label: d.Label, Short: d.Short, Aliases: append([]string{}, d.Aliases...),
			Executable: d.Executable, LoginCommand: d.LoginCommand, LoginStatus: login, Cloud: d.Cloud, NeedsAPIKey: d.NeedsAPIKey,
			DefaultBaseURL: d.DefaultBaseURL, Capabilities: append([]string{}, d.DefaultCapabilities...), Parameters: append([]string{}, d.Parameters...),
			DefaultConcurrency: d.DefaultConcurrency, MaxConcurrency: d.MaxConcurrency, Efforts: append([]string{"default"}, d.Efforts...)})
	}
	return out
}

// LoginStatus reports whether a CLI scheme's own sign-in is usable.
func (x *Connector) LoginStatus(ctx context.Context, a AdapterConfig) (backend.LoginStatus, error) {
	if err := backendRegistry.Validate(&a, true); err != nil {
		return backend.LoginStatus{}, err
	}
	b, err := backendRegistry.Open(a, backend.Dependencies{ProbeClient: x.client, ModelClient: x.modelClient})
	if err != nil {
		return backend.LoginStatus{}, err
	}
	c, ok := b.(backend.LoginChecker)
	if !ok {
		return backend.LoginStatus{}, fmt.Errorf("%s schemes have no CLI sign-in", a.Type)
	}
	return c.LoginStatus(ctx), nil
}
