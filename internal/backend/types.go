package backend

type Config struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Name            string   `json:"name"`
	Provider        string   `json:"provider,omitempty"`
	BaseURL         string   `json:"base_url,omitempty"`
	APIKey          string   `json:"api_key,omitempty"`
	Executable      string   `json:"executable,omitempty"`
	CodexBin        string   `json:"codex_bin,omitempty"`
	Models          []string `json:"models"`
	ContextWindow   int      `json:"context_window,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	IsDefault       bool     `json:"is_default,omitempty"`
	Enabled         bool     `json:"enabled"`
	SkipModelProbe  bool     `json:"skip_model_probe,omitempty"`
	// Capabilities are the per-model flags advertised for this adapter's
	// models (tools, vision, audio). Empty means the adapter type default.
	Capabilities []string `json:"capabilities,omitempty"`
	// Concurrency limits parallel tasks on this adapter (CLI backends are always 1).
	Concurrency int `json:"concurrency,omitempty"`
}

// Health is the result of probing one adapter.
type Health struct {
	AdapterID         string   `json:"adapter_id"`
	AdapterType       string   `json:"adapter_type"`
	RuntimeVersion    string   `json:"runtime_version,omitempty"`
	Models            []string `json:"models"`
	Health            string   `json:"health"` // ready | unavailable | disabled
	AuthState         string   `json:"auth_state"`
	InferenceLocation string   `json:"inference_location"` // local | provider_cloud
	FreeSlots         int      `json:"free_slots"`
	// Error says why the scheme is unavailable when that is known locally
	// (e.g. the Codex CLI is missing). Display only.
	Error string `json:"error,omitempty"`
}
