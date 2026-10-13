// Package connector is the desktop core: it manages local model adapters
// (Ollama, OpenAI-compatible, Anthropic-compatible, Codex app-server) and
// serves them to a paired remote service over protocol v2 (lsp2) as an
// lsp2 client Executor.
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/audit"
	"github.com/tyro668/LocalSubProxy/internal/durable"
	"github.com/tyro668/LocalSubProxy/internal/lsp2"
	"github.com/tyro668/LocalSubProxy/internal/lsp2/client"
)

type Config struct {
	// RemoteURL is the paired remote service (health is shown in the UI).
	RemoteURL string
	// Identity is the paired lsp2 device identity; nil means "not paired"
	// (adapters can still be managed and tested).
	Identity *client.Identity
	// DataKey is the hex AES-256 key that seals the adapter file.
	DataKey     string
	AdapterFile string
	Adapters    []AdapterConfig
	// MaxConcurrency is the number of tasks the device accepts in parallel
	// (advertised in hello; default 4). Adapters may lower it per adapter.
	MaxConcurrency int
	// Audit, if set, receives one record per served task (no prompt content).
	Audit *audit.Log
}

type AdapterConfig struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Name            string   `json:"name"`
	Provider        string   `json:"provider,omitempty"`
	BaseURL         string   `json:"base_url,omitempty"`
	APIKey          string   `json:"api_key,omitempty"`
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
	// Concurrency limits parallel tasks on this adapter (Codex is always 1).
	Concurrency int `json:"concurrency,omitempty"`
}

// AdapterHealth is the result of probing one adapter.
type AdapterHealth struct {
	AdapterID         string   `json:"adapter_id"`
	AdapterType       string   `json:"adapter_type"`
	RuntimeVersion    string   `json:"runtime_version,omitempty"`
	Models            []string `json:"models"`
	Health            string   `json:"health"` // ready | unavailable | disabled
	AuthState         string   `json:"auth_state"`
	InferenceLocation string   `json:"inference_location"` // local | provider_cloud
	FreeSlots         int      `json:"free_slots"`
}

type Connector struct {
	cfg         Config
	client      *http.Client
	modelClient *http.Client
	cipher      *durable.Cipher
	mu          sync.Mutex
	adapters    map[string]AdapterConfig
	health      map[string]AdapterHealth
	running     map[string]string // task → adapter
	remoteOK    bool
	gatewayOK   bool
	cl          *client.Client
	lastErr     string
}

func New(cfg Config) (*Connector, error) {
	if cfg.Identity != nil && !cfg.Identity.Valid() {
		return nil, errors.New("lsp2 identity is invalid")
	}
	if cfg.RemoteURL != "" {
		if err := safeURL(cfg.RemoteURL); err != nil {
			return nil, fmt.Errorf("remote service URL: %w", err)
		}
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 4
	}
	var c *durable.Cipher
	if cfg.DataKey != "" || cfg.AdapterFile != "" {
		var err error
		if c, err = durable.NewCipher(cfg.DataKey); err != nil {
			return nil, err
		}
	}
	x := &Connector{cfg: cfg, cipher: c, adapters: map[string]AdapterConfig{}, health: map[string]AdapterHealth{}, running: map[string]string{}, remoteOK: cfg.RemoteURL == "",
		client:      &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		modelClient: &http.Client{Timeout: 0, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, a := range cfg.Adapters {
		if err := validateAdapter(&a); err != nil {
			return nil, err
		}
		if _, exists := x.adapters[a.ID]; exists {
			return nil, errors.New("duplicate adapter ID")
		}
		x.adapters[a.ID] = a
	}
	if cfg.AdapterFile != "" {
		if b, err := os.ReadFile(cfg.AdapterFile); err == nil {
			var saved []AdapterConfig
			if err := c.Open(b, &saved); err != nil {
				return nil, fmt.Errorf("open adapters: %w", err)
			}
			for _, a := range saved {
				if err := validateAdapter(&a); err != nil {
					return nil, err
				}
				x.adapters[a.ID] = a
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return x, nil
}

// DeviceName is the human-readable device name shown during pairing.
func DeviceName() string { return localDeviceName() }

func safeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return errors.New("URL must contain only scheme and host")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return errors.New("HTTPS required")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return errors.New("plain HTTP is permitted only for a loopback IP")
	}
	return nil
}

func validateAdapter(a *AdapterConfig) error {
	if a.ID == "" || len(a.ID) > 64 || strings.Contains(a.ID, "/") || len(a.Models) == 0 || len(a.Models) > 100 {
		return errors.New("adapter ID (no '/') and 1-100 models required")
	}
	if len(a.Name) > 100 || len(a.Provider) > 64 || a.ContextWindow < 0 || a.ContextWindow > 2_000_000 || a.MaxOutputTokens < 0 || a.MaxOutputTokens > 1_000_000 || a.Concurrency < 0 || a.Concurrency > 16 {
		return errors.New("invalid profile settings")
	}
	switch a.ReasoningEffort {
	case "", "off", "low", "medium", "high":
	default:
		return errors.New("invalid reasoning effort")
	}
	for _, m := range a.Models {
		if m == "" || len(m) > 200 {
			return errors.New("invalid model ID")
		}
	}
	for _, c := range a.Capabilities {
		if c != lsp2.ModelCapTools && c != lsp2.ModelCapVision && c != lsp2.ModelCapAudio {
			return fmt.Errorf("unknown model capability %q", c)
		}
	}
	switch a.Type {
	case "ollama_http":
		if err := safeURL(a.BaseURL); err != nil {
			return fmt.Errorf("adapter URL: %w", err)
		}
	case "openai_compatible", "anthropic_compatible":
		if err := safeProviderURL(a.BaseURL); err != nil {
			return fmt.Errorf("adapter URL: %w", err)
		}
		if a.APIKey == "" || len(a.APIKey) > 4096 {
			return errors.New("API key required (maximum 4096 characters)")
		}
	case "codex_app_server":
		if len(a.Capabilities) > 0 {
			return errors.New("the Codex adapter is text-only")
		}
		if a.CodexBin == "" {
			a.CodexBin = "codex"
		}
		path, err := resolveCodex(a.CodexBin)
		if err != nil {
			return err
		}
		a.CodexBin = path
	default:
		return fmt.Errorf("unsupported adapter type %q", a.Type)
	}
	return nil
}

func safeProviderURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.Contains(u.Path, "..") {
		return errors.New("invalid provider URL")
	}
	u.Path = ""
	u.RawPath = ""
	return safeURL(u.String())
}

func providerEndpoint(base, endpoint string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + endpoint
	}
	return base + "/v1" + endpoint
}

func adapterEndpoint(a AdapterConfig, endpoint string) string {
	if a.Provider == "deepseek" {
		base := strings.TrimRight(a.BaseURL, "/")
		if base == "https://api.deepseek.com" || base == "https://api.deepseek.com/v1" {
			return "https://api.deepseek.com" + endpoint
		}
	}
	return providerEndpoint(a.BaseURL, endpoint)
}

// capsOf returns the per-model capability flags of an adapter.
func capsOf(a AdapterConfig) []string {
	if len(a.Capabilities) > 0 {
		return append([]string(nil), a.Capabilities...)
	}
	switch a.Type {
	case "openai_compatible", "anthropic_compatible":
		return []string{lsp2.ModelCapTools, lsp2.ModelCapVision}
	case "ollama_http":
		return []string{lsp2.ModelCapTools}
	}
	return []string{}
}

func adapterSlots(a AdapterConfig) int {
	if a.Type == "codex_app_server" {
		return 1
	}
	if a.Concurrency > 0 {
		return a.Concurrency
	}
	return 4
}

func has(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

type knownFailure string

func (e knownFailure) Error() string { return string(e) }

// ---------- lsp2 Executor ----------

// Models lists "adapterID/modelID" for enabled adapters that are not known to be unhealthy.
func (x *Connector) Models() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := []string{}
	for _, a := range x.adapters {
		if !a.Enabled {
			continue
		}
		if h, ok := x.health[a.ID]; ok && h.Health != "ready" {
			continue
		}
		for _, m := range a.Models {
			out = append(out, a.ID+"/"+m)
		}
	}
	return out
}

// ModelCapabilities advertises per-model flags (lsp2 model_capabilities).
func (x *Connector) ModelCapabilities() map[string][]string {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := map[string][]string{}
	for _, a := range x.adapters {
		for _, m := range a.Models {
			out[a.ID+"/"+m] = capsOf(a)
		}
	}
	return out
}

// Execute runs one verified (and, for E2EE tasks, already decrypted) task and
// records it in the local audit log.
func (x *Connector) Execute(ctx context.Context, run lsp2.TaskRun, emit func(lsp2.TaskDelta) error) (*lsp2.TaskDone, *lsp2.ErrorBody) {
	if x.cfg.Audit == nil {
		return x.execute(ctx, run, emit)
	}
	start := time.Now()
	var first time.Time
	done, fail := x.execute(ctx, run, func(d lsp2.TaskDelta) error {
		if first.IsZero() {
			first = time.Now()
		}
		return emit(d)
	})
	x.recordAudit(run, start, first, done, fail)
	return done, fail
}

func (x *Connector) recordAudit(run lsp2.TaskRun, start, first time.Time, done *lsp2.TaskDone, fail *lsp2.ErrorBody) {
	r := audit.Record{At: start, TaskID: run.TaskID, Model: run.Model, Status: audit.StatusOK, LatencyMS: time.Since(start).Milliseconds(),
		E2EE: run.E2EE != nil, Tools: len(run.Tools) > 0}
	r.Adapter, _, _ = strings.Cut(run.Model, "/")
	if id := x.cfg.Identity; id != nil {
		r.ConnectionID, r.Gateway, r.Tenant, r.SaaSName = id.DeviceID, id.Issuer, id.Tenant, id.SaaSName
	}
	if !first.IsZero() {
		r.TTFTMS = first.Sub(start).Milliseconds()
	}
	for _, m := range run.Messages {
		for _, p := range m.Content.Parts {
			if p.ImageURL != nil {
				r.Images++
			}
		}
	}
	if fail != nil {
		r.Status, r.ErrorCode = audit.StatusError, fail.Code
		if fail.Code == lsp2.ErrCancelled {
			r.Status = audit.StatusCancelled
		}
	} else if done != nil && done.Usage != nil {
		r.PromptTokens, r.CompletionTokens, r.UsageReported = done.Usage.PromptTokens, done.Usage.CompletionTokens, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := x.cfg.Audit.Record(ctx, r); err != nil {
		log.Printf("audit: %v", err)
	}
}

func (x *Connector) execute(ctx context.Context, run lsp2.TaskRun, emit func(lsp2.TaskDelta) error) (*lsp2.TaskDone, *lsp2.ErrorBody) {
	adapterID, modelID, ok := strings.Cut(run.Model, "/")
	x.mu.Lock()
	a, exists := x.adapters[adapterID]
	if !ok || !exists || !a.Enabled || !has(a.Models, modelID) {
		x.mu.Unlock()
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrForbidden, Message: "model not allowed by local adapter policy"}
	}
	busy := 0
	for _, id := range x.running {
		if id == adapterID {
			busy++
		}
	}
	if busy >= adapterSlots(a) {
		x.mu.Unlock()
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrDeviceBusy, Message: "adapter is at its concurrency limit", Retryable: true}
	}
	x.running[run.TaskID] = adapterID
	x.mu.Unlock()
	defer func() { x.mu.Lock(); delete(x.running, run.TaskID); x.mu.Unlock() }()
	if fail := checkCaps(a, run); fail != nil {
		return nil, fail
	}
	ctx, cancel := context.WithTimeout(ctx, 600*time.Second)
	defer cancel()
	out := &limitedEmitter{emit: emit}
	var done *lsp2.TaskDone
	var err error
	switch a.Type {
	case "codex_app_server":
		err = x.callCodex(ctx, modelID, run.Messages, a, out.text)
		done = &lsp2.TaskDone{FinishReason: "stop"}
	case "openai_compatible":
		done, err = x.callOpenAI(ctx, modelID, run, a, out)
	case "anthropic_compatible":
		done, err = x.callAnthropic(ctx, modelID, run, a, out)
	default:
		done, err = x.callOllama(ctx, modelID, run, a, out)
	}
	switch {
	case err == nil:
		return done, nil
	case errors.Is(err, errCodexInterrupted), errors.Is(err, context.Canceled):
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrCancelled, Message: "cancelled"}
	case errors.Is(err, context.DeadlineExceeded):
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrTimeout, Message: "runtime deadline elapsed"}
	}
	var known knownFailure
	if errors.As(err, &known) {
		switch {
		case strings.HasPrefix(err.Error(), "POLICY_DENIED"):
			return nil, &lsp2.ErrorBody{Code: lsp2.ErrForbidden, Message: err.Error()}
		case strings.HasPrefix(err.Error(), "CAPABILITY_UNSUPPORTED"):
			return nil, &lsp2.ErrorBody{Code: lsp2.ErrUnsupported, Message: err.Error()}
		}
	}
	retry := errors.As(err, &known) && (strings.HasPrefix(err.Error(), "PROVIDER_RATE_LIMITED") || strings.HasPrefix(err.Error(), "QUOTA_EXHAUSTED"))
	return nil, &lsp2.ErrorBody{Code: lsp2.ErrUpstream, Message: err.Error(), Retryable: retry}
}

// checkCaps rejects inputs the adapter cannot represent (the gateway checks
// the advertised flags first; this is the device's own enforcement).
func checkCaps(a AdapterConfig, run lsp2.TaskRun) *lsp2.ErrorBody {
	caps := capsOf(a)
	unsupported := func(msg string) *lsp2.ErrorBody {
		return &lsp2.ErrorBody{Code: lsp2.ErrUnsupported, Message: msg}
	}
	if (len(run.Tools) > 0 || len(run.ToolChoice) > 0) && !has(caps, lsp2.ModelCapTools) {
		return unsupported("model does not support tools")
	}
	for _, m := range run.Messages {
		if (m.Role == "tool" || len(m.ToolCalls) > 0) && !has(caps, lsp2.ModelCapTools) {
			return unsupported("model does not support tool messages")
		}
		for _, p := range m.Content.Parts {
			switch {
			case p.ImageURL != nil && !has(caps, lsp2.ModelCapVision):
				return unsupported("model does not accept images")
			case p.ImageURL != nil && a.Type == "ollama_http" && !strings.HasPrefix(p.ImageURL.URL, "data:"):
				return unsupported("Ollama accepts inline (data:) images only")
			case p.InputAudio != nil && (!has(caps, lsp2.ModelCapAudio) || a.Type != "openai_compatible"):
				return unsupported("model does not accept audio")
			}
		}
	}
	return nil
}

// limitedEmitter caps total output and splits large text deltas.
type limitedEmitter struct {
	emit  func(lsp2.TaskDelta) error
	total int
}

const maxOutputBytes = 10 << 20

func (l *limitedEmitter) delta(d lsp2.TaskDelta) error {
	l.total += len(d.Text)
	for _, tc := range d.ToolCalls {
		l.total += len(tc.Function.Arguments) + len(tc.Function.Name)
	}
	if l.total > maxOutputBytes {
		return knownFailure("OUTPUT_LIMIT_EXCEEDED")
	}
	for len(d.Text) > 32<<10 {
		n := 32 << 10
		for n > 0 && (d.Text[n]&0xC0) == 0x80 {
			n--
		}
		if err := l.emit(lsp2.TaskDelta{Text: d.Text[:n]}); err != nil {
			return err
		}
		d.Text = d.Text[n:]
	}
	if d.Text == "" && len(d.ToolCalls) == 0 {
		return nil
	}
	return l.emit(d)
}

func (l *limitedEmitter) text(s string) error { return l.delta(lsp2.TaskDelta{Text: s}) }

// ---------- session ----------

// Run serves the adapters over lsp2 until ctx ends or the device is revoked.
func (x *Connector) Run(ctx context.Context) error {
	if x.cfg.Identity == nil {
		return errors.New("device is not paired")
	}
	cl, err := client.New(client.Config{Executor: x, MaxConcurrency: x.cfg.MaxConcurrency, Client: "localsubproxy-desktop/lsp2",
		OnState: func(ok bool) { x.mu.Lock(); x.gatewayOK = ok; x.mu.Unlock() }}, *x.cfg.Identity)
	if err != nil {
		return err
	}
	x.mu.Lock()
	x.cl = cl
	x.mu.Unlock()
	if x.cfg.RemoteURL != "" {
		go x.watchRemote(ctx)
	}
	go func() { // keep adapter health fresh and re-announce when the model list changes
		last := ""
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			x.probeAll(pctx)
			cancel()
			if now := strings.Join(x.Models(), ","); now != last {
				if last != "" {
					cl.ModelsChanged()
				}
				last = now
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	err = cl.Run(ctx, nil)
	x.mu.Lock()
	x.gatewayOK = false
	if err != nil && ctx.Err() == nil {
		x.lastErr = err.Error()
	}
	x.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		log.Printf("lsp2 session stopped: %v", err)
	}
	return err
}

// notifyModels re-announces models after adapter edits or health changes.
func (x *Connector) notifyModels() {
	x.mu.Lock()
	cl := x.cl
	x.mu.Unlock()
	if cl != nil {
		cl.ModelsChanged()
	}
}

func (x *Connector) checkRemote(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(x.cfg.RemoteURL, "/")+"/.well-known/lsp-connector", nil)
	if err != nil {
		return err
	}
	resp, err := x.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote service discovery: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (x *Connector) watchRemote(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := x.checkRemote(probe)
		cancel()
		x.mu.Lock()
		x.remoteOK = err == nil
		x.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ---------- probes ----------

func (x *Connector) probeAdapter(ctx context.Context, a AdapterConfig) AdapterHealth {
	if a.ID == "" {
		return AdapterHealth{}
	}
	if a.Type == "codex_app_server" {
		return x.withSlots(x.probeCodexAdapter(ctx, a))
	}
	h := AdapterHealth{AdapterID: a.ID, AdapterType: a.Type, Models: a.Models, Health: "unavailable", AuthState: "ready", InferenceLocation: "local", FreeSlots: adapterSlots(a)}
	cloud := a.Type == "openai_compatible" || a.Type == "anthropic_compatible"
	if cloud {
		h.InferenceLocation, h.AuthState = "provider_cloud", "configured"
	}
	if !a.Enabled {
		h.Health, h.FreeSlots = "disabled", 0
		return h
	}
	if cloud && a.SkipModelProbe {
		h.Health = "ready"
		return x.withSlots(h)
	}
	probeURL := strings.TrimRight(a.BaseURL, "/") + "/api/tags"
	if cloud {
		probeURL = adapterEndpoint(a, "/models")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", probeURL, nil)
	if err != nil {
		return h
	}
	if a.Type == "openai_compatible" {
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	} else if a.Type == "anthropic_compatible" {
		req.Header.Set("x-api-key", a.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	probeClient := *x.client
	probeClient.Timeout = 5 * time.Second
	resp, err := probeClient.Do(req)
	if err != nil {
		return h
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return h
	}
	available := map[string]bool{}
	if cloud {
		var v struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v) != nil {
			return h
		}
		for _, m := range v.Data {
			available[m.ID] = true
		}
	} else {
		var v struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v) != nil {
			return h
		}
		for _, m := range v.Models {
			available[m.Name], available[m.Model] = true, true
		}
	}
	h.Models = nil
	for _, m := range a.Models {
		if available[m] {
			h.Models = append(h.Models, m)
		}
	}
	if len(h.Models) > 0 {
		h.Health = "ready"
	}
	return x.withSlots(h)
}

func (x *Connector) withSlots(h AdapterHealth) AdapterHealth {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, id := range x.running {
		if id == h.AdapterID && h.FreeSlots > 0 {
			h.FreeSlots--
		}
	}
	return h
}

func (x *Connector) probeAll(ctx context.Context) []AdapterHealth {
	x.mu.Lock()
	list := make([]AdapterConfig, 0, len(x.adapters))
	for _, a := range x.adapters {
		list = append(list, a)
	}
	x.mu.Unlock()
	out := make([]AdapterHealth, len(list))
	var wg sync.WaitGroup
	for i, a := range list {
		wg.Add(1)
		go func(i int, a AdapterConfig) { defer wg.Done(); out[i] = x.probeAdapter(ctx, a) }(i, a)
	}
	wg.Wait()
	x.mu.Lock()
	x.health = map[string]AdapterHealth{}
	for _, h := range out {
		x.health[h.AdapterID] = h
	}
	x.mu.Unlock()
	return out
}
