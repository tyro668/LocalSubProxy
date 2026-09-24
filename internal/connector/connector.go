package connector

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"localsubproxy/internal/durable"
	"localsubproxy/internal/protocol"
	"localsubproxy/internal/server"
)

type Config struct {
	ServerURL, RemoteURL, DeviceID, DeviceToken, AdapterID, AdapterType, OllamaURL, CodexBin, JournalFile, JournalKey string
	AllowedModels                                                                                                     []string
	MaxJournalBytes                                                                                                   int64
	Adapters                                                                                                          []AdapterConfig
	AdapterFile                                                                                                       string
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
}

type localTask struct {
	Command     protocol.Command `json:"command"`
	State       string           `json:"state"`
	StartIntent bool             `json:"start_intent"`
	LeaseUntil  time.Time        `json:"lease_until,omitempty"`
	AckSeq      int64            `json:"ack_seq"`
	NextSeq     int64            `json:"next_seq,omitempty"`
	DoneAt      time.Time        `json:"done_at,omitempty"`
	Events      []protocol.Event `json:"events"`
}
type journal struct {
	Tasks map[string]*localTask `json:"tasks"`
}

type Connector struct {
	cfg               Config
	client            *http.Client
	modelClient       *http.Client
	cipher            *durable.Cipher
	mu                sync.Mutex
	j                 journal
	running           map[string]context.CancelFunc
	leaseStopped      map[string]bool
	journalBytes      int64
	runWG             sync.WaitGroup
	wsMu              sync.Mutex
	ws                *wsClient
	wsDialer          *websocket.Dialer
	adapters          map[string]AdapterConfig
	health            map[string]protocol.AdapterHealth
	lastProbeVersion  int64
	lastHealthVersion int64
	heartbeatSnapshot protocol.Heartbeat
	heartbeatOK       bool
	pollFailed        bool
	remoteOK          bool
	online            bool
}

func New(cfg Config) (*Connector, error) {
	if cfg.JournalFile == "" || (cfg.ServerURL == "" && cfg.DeviceToken != "") || (cfg.ServerURL != "" && (cfg.DeviceID == "" || len(cfg.DeviceToken) < 32)) {
		return nil, errors.New("journal and valid connection credentials are required")
	}
	if cfg.ServerURL != "" {
		if err := safeURL(cfg.ServerURL); err != nil {
			return nil, fmt.Errorf("server URL: %w", err)
		}
	}
	if cfg.RemoteURL != "" {
		if err := safeURL(cfg.RemoteURL); err != nil {
			return nil, fmt.Errorf("remote service URL: %w", err)
		}
	}
	if cfg.AdapterType == "" {
		cfg.AdapterType = "ollama_http"
	}
	if cfg.MaxJournalBytes == 0 {
		cfg.MaxJournalBytes = 64 << 20
	}
	if cfg.MaxJournalBytes < 256<<10 {
		return nil, errors.New("journal limit must be at least 256 KiB")
	}
	if cfg.AdapterID != "" {
		found := false
		for _, a := range cfg.Adapters {
			if a.ID == cfg.AdapterID {
				found = true
				break
			}
		}
		if !found {
			if cfg.AdapterType == "" {
				cfg.AdapterType = "ollama_http"
			}
			base := cfg.OllamaURL
			if base == "" && cfg.AdapterType == "ollama_http" {
				base = "http://127.0.0.1:11434"
			}
			cfg.Adapters = append(cfg.Adapters, AdapterConfig{ID: cfg.AdapterID, Type: cfg.AdapterType, BaseURL: base, CodexBin: cfg.CodexBin, Models: cfg.AllowedModels, Enabled: true})
		}
	}
	c, err := durable.NewCipher(cfg.JournalKey)
	if err != nil {
		return nil, err
	}
	x := &Connector{cfg: cfg, cipher: c, j: journal{Tasks: map[string]*localTask{}}, adapters: map[string]AdapterConfig{}, health: map[string]protocol.AdapterHealth{}, running: map[string]context.CancelFunc{}, leaseStopped: map[string]bool{}, remoteOK: cfg.RemoteURL == "", client: &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, modelClient: &http.Client{Timeout: 0, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
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
	if b, err := os.ReadFile(cfg.JournalFile); err == nil {
		if err := c.Open(b, &x.j); err != nil {
			return nil, fmt.Errorf("open journal: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if x.j.Tasks == nil {
		x.j.Tasks = map[string]*localTask{}
	}
	// A model call may already have reached Ollama when the process died. Never start it again.
	for id, t := range x.j.Tasks {
		if t.NextSeq < t.AckSeq {
			t.NextSeq = t.AckSeq
		}
		for _, ev := range t.Events {
			if ev.Seq > t.NextSeq {
				t.NextSeq = ev.Seq
			}
		}
		var pending []protocol.Event
		for _, ev := range t.Events {
			if ev.Seq > t.AckSeq {
				pending = append(pending, ev)
			}
		}
		t.Events = pending
		if isTerminal(t.State) && t.DoneAt.IsZero() {
			t.DoneAt = time.Now().UTC()
		}
		if isTerminal(t.State) && t.AckSeq == t.NextSeq {
			t.Command.Request.Input.Messages = nil
		}
		if isTerminal(t.State) && t.AckSeq == t.NextSeq && time.Since(t.DoneAt) > 7*24*time.Hour {
			delete(x.j.Tasks, id)
			continue
		}
		if t.StartIntent && !isTerminal(t.State) {
			t.State = "UNKNOWN"
			t.DoneAt = time.Now().UTC()
			x.appendLocked(t, "task.unknown", map[string]string{"reason": "connector restarted after start intent; upstream result unknown"})
		}
	}
	if err := x.saveLocked(); err != nil {
		return nil, err
	}
	return x, nil
}

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
	if a.ID == "" || len(a.ID) > 64 || len(a.Models) == 0 || len(a.Models) > 100 {
		return errors.New("adapter ID and 1-100 models required")
	}
	if len(a.Name) > 100 || len(a.Provider) > 64 || a.ContextWindow < 0 || a.ContextWindow > 2_000_000 || a.MaxOutputTokens < 0 || a.MaxOutputTokens > 1_000_000 {
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
func isTerminal(s string) bool {
	switch s {
	case "SUCCEEDED", "FAILED", "CANCELLED", "UNKNOWN":
		return true
	}
	return false
}
func (x *Connector) saveLocked() error {
	return x.saveLockedLimit(x.cfg.MaxJournalBytes)
}

var errJournalFull = knownFailure("LOCAL_SPOOL_FULL")

func (x *Connector) saveLockedLimit(limit int64) error {
	b, err := x.cipher.Seal(x.j)
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return errJournalFull
	}
	if err := durable.Write(x.cfg.JournalFile, b); err != nil {
		return err
	}
	x.journalBytes = int64(len(b))
	return nil
}
func (x *Connector) appendLocked(t *localTask, kind string, data any) {
	t.NextSeq++
	t.Events = append(t.Events, protocol.NewEvent(t.NextSeq, kind, data))
}
func (x *Connector) appendSavedLocked(t *localTask, kind string, data any) error {
	beforeSeq, beforeLen := t.NextSeq, len(t.Events)
	x.appendLocked(t, kind, data)
	limit := x.cfg.MaxJournalBytes
	if kind == "output.delta" {
		limit -= 128 << 10
	}
	if err := x.saveLockedLimit(limit); err != nil {
		t.NextSeq, t.Events = beforeSeq, t.Events[:beforeLen]
		return err
	}
	return nil
}

func (x *Connector) api(ctx context.Context, verb, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, verb, strings.TrimRight(x.cfg.ServerURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+x.cfg.DeviceToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := x.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("server status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return nil
}

func (x *Connector) markHeartbeat(ok bool) {
	x.mu.Lock()
	x.heartbeatOK = ok
	x.updateOnlineLocked()
	x.mu.Unlock()
}

func (x *Connector) markPoll(ok bool) {
	x.mu.Lock()
	x.pollFailed = !ok
	x.updateOnlineLocked()
	x.mu.Unlock()
}

func (x *Connector) markRemote(ok bool) {
	x.mu.Lock()
	x.remoteOK = ok
	x.updateOnlineLocked()
	x.mu.Unlock()
}

func (x *Connector) updateOnlineLocked() {
	x.online = x.remoteOK && x.heartbeatOK && !x.pollFailed
}

func (x *Connector) checkRemote(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(x.cfg.RemoteURL, "/")+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := x.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote service health: HTTP %d", resp.StatusCode)
	}
	var health struct {
		Status string `json:"status"`
		Data   struct {
			APIBaseURL string `json:"apiBaseUrl"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health); err != nil {
		return err
	}
	if health.Status != "ok" && health.Data.APIBaseURL == "" {
		return errors.New("invalid remote service health response")
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
		x.markRemote(err == nil)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (x *Connector) watchGateway(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			x.mu.Lock()
			hb := x.heartbeatSnapshot
			x.mu.Unlock()
			if hb.Protocol == "" {
				continue
			}
			probe, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = x.sendHeartbeat(probe, hb)
			cancel()
		}
	}
}

func (x *Connector) probe(ctx context.Context) protocol.AdapterHealth {
	x.mu.Lock()
	a := x.adapters[x.cfg.AdapterID]
	if a.ID == "" {
		for _, value := range x.adapters {
			a = value
			break
		}
	}
	x.mu.Unlock()
	return x.probeAdapter(ctx, a)
}
func (x *Connector) probeAdapter(ctx context.Context, a AdapterConfig) protocol.AdapterHealth {
	if a.ID == "" {
		return protocol.AdapterHealth{}
	}
	if a.Type == "codex_app_server" {
		return x.withStorageHealth(x.probeCodexAdapter(ctx, a))
	}
	h := protocol.AdapterHealth{AdapterID: a.ID, AdapterType: a.Type, Models: a.Models, Operations: []string{"chat.generate"}, Health: "unavailable", AuthState: "ready", AccountScope: "personal", InferenceLocation: "local", FreeSlots: 1}
	if a.Type == "openai_compatible" || a.Type == "anthropic_compatible" {
		h.InferenceLocation = "provider_cloud"
		h.AuthState = "configured"
	}
	if !a.Enabled {
		h.Health = "disabled"
		h.FreeSlots = 0
		return h
	}
	if (a.Type == "openai_compatible" || a.Type == "anthropic_compatible") && a.SkipModelProbe {
		h.Health = "ready"
		return x.withStorageHealth(h)
	}
	endpoint := "/api/tags"
	if a.Type == "openai_compatible" || a.Type == "anthropic_compatible" {
		endpoint = "/models"
	}
	probeURL := strings.TrimRight(a.BaseURL, "/") + endpoint
	if a.Type == "openai_compatible" || a.Type == "anthropic_compatible" {
		probeURL = adapterEndpoint(a, endpoint)
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
	if a.Type == "openai_compatible" || a.Type == "anthropic_compatible" {
		var v struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v) != nil {
			return h
		}
		available := map[string]bool{}
		for _, m := range v.Data {
			available[m.ID] = true
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
		return x.withStorageHealth(h)
	}
	var v struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return h
	}
	available := map[string]bool{}
	for _, m := range v.Models {
		available[m.Name] = true
		available[m.Model] = true
	}
	var models []string
	for _, name := range a.Models {
		if available[name] {
			models = append(models, name)
		}
	}
	h.Models = models
	if len(models) > 0 {
		h.Health = "ready"
	}
	x.mu.Lock()
	if len(x.running) > 0 {
		h.FreeSlots = 0
	}
	x.mu.Unlock()
	return x.withStorageHealth(h)
}

func (x *Connector) probeAll(ctx context.Context) protocol.Heartbeat {
	x.mu.Lock()
	version := time.Now().UnixNano()
	if version <= x.lastProbeVersion {
		version = x.lastProbeVersion + 1
	}
	x.lastProbeVersion = version
	list := make([]AdapterConfig, 0, len(x.adapters))
	for _, a := range x.adapters {
		list = append(list, a)
	}
	x.mu.Unlock()
	hb := protocol.Heartbeat{Protocol: protocol.Version, DeviceID: x.cfg.DeviceID, SnapshotVersion: version}
	hb.Adapters = make([]protocol.AdapterHealth, len(list))
	var wg sync.WaitGroup
	for i, a := range list {
		wg.Add(1)
		go func(i int, a AdapterConfig) { defer wg.Done(); hb.Adapters[i] = x.probeAdapter(ctx, a) }(i, a)
	}
	wg.Wait()
	x.mu.Lock()
	if version >= x.lastHealthVersion {
		x.health = map[string]protocol.AdapterHealth{}
		for _, h := range hb.Adapters {
			x.health[h.AdapterID] = h
		}
		x.lastHealthVersion = version
	}
	x.mu.Unlock()
	if len(hb.Adapters) > 0 {
		hb.Adapter = hb.Adapters[0]
	}
	x.mu.Lock()
	if hb.SnapshotVersion >= x.heartbeatSnapshot.SnapshotVersion {
		x.heartbeatSnapshot = hb
	}
	x.mu.Unlock()
	return hb
}

func (x *Connector) withStorageHealth(h protocol.AdapterHealth) protocol.AdapterHealth {
	x.mu.Lock()
	if len(x.running) > 0 {
		h.FreeSlots = 0
	}
	if x.journalBytes > x.cfg.MaxJournalBytes-(128<<10) {
		h.Health = "storage_full"
		h.FreeSlots = 0
	}
	x.mu.Unlock()
	return h
}

func (x *Connector) heartbeat(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	h := x.probeAll(probeCtx)
	cancel()
	return x.sendHeartbeat(ctx, h)
}

func (x *Connector) sendHeartbeat(ctx context.Context, h protocol.Heartbeat) error {
	var response struct {
		Status string `json:"status"`
	}
	err := x.api(ctx, http.MethodPost, "/v1/connector/heartbeat", h, &response)
	if err == nil && response.Status != "ok" {
		err = errors.New("invalid gateway heartbeat response")
	}
	x.markHeartbeat(err == nil)
	return err
}

func (x *Connector) Run(ctx context.Context) error {
	if x.cfg.ServerURL == "" {
		return errors.New("remote service connection is not configured")
	}
	defer x.markHeartbeat(false)
	go x.leaseLoop(ctx)
	go x.watchGateway(ctx)
	if x.cfg.RemoteURL != "" {
		go x.watchRemote(ctx)
	}
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
			if err := x.heartbeat(ctx); err != nil && ctx.Err() == nil {
				log.Printf("heartbeat: %v", err)
			}
		}
	}()
	backoff := time.Second
	for ctx.Err() == nil {
		connectedAt := time.Now()
		hbCtx, hbCancel := context.WithTimeout(ctx, 7*time.Second)
		if err := x.heartbeat(hbCtx); err != nil && hbCtx.Err() == nil {
			log.Printf("HTTP heartbeat: %v", err)
		}
		hbCancel()
		err := x.pollHTTP(ctx)
		if ctx.Err() != nil {
			break
		}
		if time.Since(connectedAt) >= 25*time.Second {
			backoff = time.Second
		}
		log.Printf("HTTP command poll: %v", err)
		jitter := time.Duration(time.Now().UnixNano() % int64(backoff/4+1))
		timer := time.NewTimer(backoff + jitter)
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
	x.mu.Lock()
	for _, cancel := range x.running {
		cancel()
	}
	x.mu.Unlock()
	x.runWG.Wait()
	return ctx.Err()
}

func (x *Connector) leaseLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			x.renewLeases(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (x *Connector) renewLeases(ctx context.Context) {
	type leaseTask struct {
		command protocol.Command
		until   time.Time
	}
	x.mu.Lock()
	var active []leaseTask
	for id, t := range x.j.Tasks {
		if x.running[id] != nil && t.StartIntent && !isTerminal(t.State) {
			active = append(active, leaseTask{t.Command, t.LeaseUntil})
		}
	}
	x.mu.Unlock()
	for _, item := range active {
		if !item.until.IsZero() && !time.Now().Before(item.until) {
			x.mu.Lock()
			x.leaseStopped[item.command.TaskID] = true
			stop := x.running[item.command.TaskID]
			x.mu.Unlock()
			if stop != nil {
				stop()
			}
			continue
		}
		if item.until.IsZero() || time.Until(item.until) > 90*time.Second {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var response struct {
			LeaseUntil time.Time `json:"lease_until"`
		}
		err := x.api(requestCtx, http.MethodPost, "/v1/connector/lease", map[string]any{"task_id": item.command.TaskID, "attempt_id": item.command.AttemptID, "fence_token": item.command.FenceToken}, &response)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("lease renewal pending for %s: %v", item.command.TaskID, err)
			}
			continue
		}
		if !response.LeaseUntil.After(time.Now()) {
			continue
		}
		x.mu.Lock()
		if t := x.j.Tasks[item.command.TaskID]; t != nil && t.Command.AttemptID == item.command.AttemptID && response.LeaseUntil.After(t.LeaseUntil) {
			before := t.LeaseUntil
			t.LeaseUntil = response.LeaseUntil
			if err := x.saveLocked(); err != nil {
				t.LeaseUntil = before
				log.Printf("lease journal: %v", err)
			}
		}
		x.mu.Unlock()
	}
}

func (x *Connector) pollHTTP(ctx context.Context) error {
	if err := x.resumeAccepted(ctx); err != nil {
		log.Printf("accepted task recovery: %v", err)
	}
	if err := x.sendPending(ctx); err != nil {
		log.Printf("event sync: %v", err)
	}
	for ctx.Err() == nil {
		var cmd protocol.Command
		if err := x.api(ctx, http.MethodGet, "/v1/connector/commands", nil, &cmd); err != nil {
			x.markPoll(false)
			return err
		}
		x.markPoll(true)
		if cmd.Type != "noop" {
			if err := x.handle(ctx, cmd); err != nil {
				log.Printf("command rejected: %v", err)
			}
		}
	}
	return ctx.Err()
}

func (x *Connector) resumeAccepted(ctx context.Context) error {
	x.mu.Lock()
	var pending []protocol.Command
	for id, t := range x.j.Tasks {
		if t.State == "ACCEPTED" && !t.StartIntent && x.running[id] == nil {
			pending = append(pending, t.Command)
		}
	}
	x.mu.Unlock()
	for _, c := range pending {
		if err := x.accept(ctx, c); err != nil {
			return err
		}
		x.startLocal(c)
	}
	return nil
}

func (x *Connector) startLocal(c protocol.Command) {
	runCtx, cancel := context.WithTimeout(context.Background(), time.Duration(c.Request.Options.MaxRuntimeSeconds)*time.Second)
	x.mu.Lock()
	if x.running[c.TaskID] == nil {
		x.running[c.TaskID] = cancel
		x.runWG.Add(1)
		go func() { defer x.runWG.Done(); x.execute(runCtx, c) }()
	} else {
		cancel()
	}
	x.mu.Unlock()
}

func (x *Connector) handle(ctx context.Context, c protocol.Command) error {
	if c.Protocol != protocol.Version || c.DeviceID != x.cfg.DeviceID || !server.VerifyCommand(c, x.cfg.DeviceToken) {
		return errors.New("invalid command authorization")
	}
	if time.Now().After(c.ExpiresAt) {
		return errors.New("expired command")
	}
	switch c.Type {
	case "task.cancel":
		x.mu.Lock()
		t := x.j.Tasks[c.TaskID]
		cancel := x.running[c.TaskID]
		if t != nil && t.Command.AttemptID == c.AttemptID && t.Command.FenceToken == c.FenceToken && cancel == nil && !isTerminal(t.State) {
			before := t.State
			t.State = "CANCELLED"
			t.DoneAt = time.Now().UTC()
			if err := x.appendSavedLocked(t, "task.cancelled", map[string]string{"cancel_outcome": "stopped_before_start"}); err != nil {
				t.State = before
				t.DoneAt = time.Time{}
				x.mu.Unlock()
				return err
			}
		}
		x.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return x.sendPending(ctx)
	case "task.dispatch":
		x.mu.Lock()
		a, exists := x.adapters[c.Request.Target.AdapterID]
		x.mu.Unlock()
		operationAllowed := (a.Type == "ollama_http" || a.Type == "openai_compatible" || a.Type == "anthropic_compatible") && c.Request.Operation == "chat.generate" || a.Type == "codex_app_server" && c.Request.Operation == "agent.run"
		if c.Digest != protocol.Digest(c.Request) || !exists || !a.Enabled || !operationAllowed || c.Request.Target.DeviceID != x.cfg.DeviceID || !has(a.Models, c.Request.Target.ModelID) {
			return errors.New("task violates local adapter policy")
		}
		x.mu.Lock()
		if old := x.j.Tasks[c.TaskID]; old != nil {
			if old.Command.AttemptID != c.AttemptID || old.Command.Digest != c.Digest || old.Command.FenceToken != c.FenceToken {
				x.mu.Unlock()
				return errors.New("task replay conflict")
			}
			resume := old.State == "ACCEPTED" && !old.StartIntent && x.running[c.TaskID] == nil
			x.mu.Unlock()
			if err := x.accept(ctx, c); err != nil {
				return err
			}
			if resume {
				x.startLocal(c)
			}
			return nil
		}
		if len(x.running) > 0 {
			x.mu.Unlock()
			return errors.New("device busy")
		}
		if x.journalBytes > x.cfg.MaxJournalBytes-(128<<10) {
			x.mu.Unlock()
			return errJournalFull
		}
		t := &localTask{Command: c, State: "ACCEPTED"}
		x.j.Tasks[c.TaskID] = t
		if err := x.saveLocked(); err != nil {
			delete(x.j.Tasks, c.TaskID)
			x.mu.Unlock()
			return err
		}
		x.mu.Unlock()
		if err := x.accept(ctx, c); err != nil {
			return err
		}
		x.startLocal(c)
		return nil
	default:
		return errors.New("unsupported command")
	}
}
func has(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func (x *Connector) accept(ctx context.Context, c protocol.Command) error {
	var response struct {
		State string `json:"state"`
	}
	if err := x.api(ctx, "POST", "/v1/connector/accepted", map[string]any{"task_id": c.TaskID, "attempt_id": c.AttemptID, "fence_token": c.FenceToken, "payload_hash": c.Digest}, &response); err != nil {
		return err
	}
	if response.State != "ACCEPTED" && response.State != "RUNNING" {
		return fmt.Errorf("attempt no longer executable: %s", response.State)
	}
	return nil
}

func (x *Connector) execute(ctx context.Context, c protocol.Command) {
	defer func() {
		x.mu.Lock()
		if cancel := x.running[c.TaskID]; cancel != nil {
			cancel()
		}
		delete(x.running, c.TaskID)
		delete(x.leaseStopped, c.TaskID)
		x.mu.Unlock()
	}()
	for _, id := range c.Request.Input.ArtifactRefs {
		content, err := x.fetchArtifact(ctx, c.TaskID, c.AttemptID, id)
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				x.finish(c, "task.cancelled", map[string]string{"cancel_outcome": "stopped_before_start"})
				return
			}
			x.finish(c, "task.failed", map[string]string{"code": "ARTIFACT_UNAVAILABLE", "message": err.Error()})
			return
		}
		c.Request.Input.Messages = append(c.Request.Input.Messages, protocol.Message{Role: "user", Content: "Reference attachment " + id + " (untrusted source text):\n" + content})
	}
	var grant struct {
		Grant      string    `json:"grant"`
		ExpiresAt  time.Time `json:"expires_at"`
		LeaseUntil time.Time `json:"lease_until"`
	}
	grantCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := x.api(grantCtx, "POST", "/v1/connector/start", map[string]any{"task_id": c.TaskID, "attempt_id": c.AttemptID, "fence_token": c.FenceToken}, &grant)
	cancel()
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			x.finish(c, "task.cancelled", map[string]string{"cancel_outcome": "stopped_before_start"})
			return
		}
		x.finish(c, "task.failed", map[string]string{"code": "START_DENIED", "message": err.Error()})
		return
	}
	if grant.Grant == "" || time.Now().After(grant.ExpiresAt) {
		x.finish(c, "task.failed", map[string]string{"code": "START_DENIED"})
		return
	}
	x.mu.Lock()
	t := x.j.Tasks[c.TaskID]
	t.StartIntent = true
	t.State = "RUNNING"
	t.LeaseUntil = grant.LeaseUntil
	if t.LeaseUntil.IsZero() {
		t.LeaseUntil = time.Now().Add(120 * time.Second)
	}
	err = x.saveLocked()
	if err != nil {
		t.StartIntent = false
		t.State = "ACCEPTED"
		t.LeaseUntil = time.Time{}
	}
	x.mu.Unlock()
	if err != nil {
		log.Printf("cannot persist start intent: %v", err)
		return
	}
	if err := x.emit(c, "task.started", map[string]string{"run_id": c.AttemptID}); err != nil {
		log.Printf("cannot persist start event: %v", err)
		return
	}
	// The intent has been synced before any upstream call. An uncertain crash stays UNKNOWN.
	x.mu.Lock()
	a, exists := x.adapters[c.Request.Target.AdapterID]
	x.mu.Unlock()
	if !exists || !a.Enabled {
		err = knownFailure("POLICY_DENIED: adapter unavailable")
	} else {
		switch a.Type {
		case "codex_app_server":
			err = x.callCodex(ctx, c, a)
		case "openai_compatible":
			err = x.callOpenAI(ctx, c, a)
		case "anthropic_compatible":
			err = x.callAnthropic(ctx, c, a)
		default:
			err = x.callOllama(ctx, c, a)
		}
	}
	x.mu.Lock()
	local := x.j.Tasks[c.TaskID]
	leaseExpired := x.leaseStopped[c.TaskID] || (local != nil && !local.LeaseUntil.IsZero() && !time.Now().Before(local.LeaseUntil))
	x.mu.Unlock()
	if leaseExpired {
		x.finish(c, "task.unknown", map[string]string{"code": "LEASE_EXPIRED", "message": "execution lease expired before confirmed upstream stop"})
		return
	}
	if errors.Is(err, errCodexInterrupted) {
		x.finish(c, "task.cancelled", map[string]string{"cancel_outcome": "confirmed_interrupted"})
		return
	}
	if errors.Is(err, context.Canceled) {
		if a.Type == "codex_app_server" {
			x.finish(c, "task.unknown", map[string]string{"code": "UPSTREAM_RESULT_UNKNOWN", "cancel_outcome": "requested_unconfirmed"})
			return
		}
		x.finish(c, "task.cancelled", map[string]string{"cancel_outcome": "local_request_closed"})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		x.finish(c, "task.unknown", map[string]string{"code": "UPSTREAM_RESULT_UNKNOWN", "message": "runtime deadline elapsed"})
		return
	}
	if err != nil {
		var known knownFailure
		if errors.As(err, &known) {
			code := "UPSTREAM_ERROR"
			for _, candidate := range []string{"AUTH_REQUIRED", "CAPABILITY_UNSUPPORTED", "POLICY_DENIED", "OUTPUT_LIMIT_EXCEEDED", "LOCAL_SPOOL_FULL", "PROTOCOL_MISMATCH", "QUOTA_EXHAUSTED", "PROVIDER_RATE_LIMITED"} {
				if strings.HasPrefix(err.Error(), candidate+":") || err.Error() == candidate {
					code = candidate
					break
				}
			}
			x.finish(c, "task.failed", map[string]string{"code": code, "message": err.Error()})
		} else {
			x.finish(c, "task.unknown", map[string]string{"code": "UPSTREAM_RESULT_UNKNOWN", "message": err.Error()})
		}
		return
	}
	x.finish(c, "task.succeeded", map[string]any{"completion_reason": "stop", "usage": nil, "usage_source": "unknown"})
}

func (x *Connector) fetchArtifact(ctx context.Context, taskID, attemptID, id string) (string, error) {
	path := "/v1/connector/artifacts/" + id + "?task_id=" + url.QueryEscape(taskID) + "&attempt_id=" + url.QueryEscape(attemptID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(x.cfg.ServerURL, "/")+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+x.cfg.DeviceToken)
	res, err := x.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("artifact %s unavailable: HTTP %d", id, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(b) == 0 || len(b) > 1<<20 || !utf8.Valid(b) {
		return "", errors.New("invalid artifact data")
	}
	hash := sha256.Sum256(b)
	if !strings.EqualFold(hex.EncodeToString(hash[:]), res.Header.Get("X-Content-SHA256")) {
		return "", errors.New("artifact digest mismatch")
	}
	return string(b), nil
}

func (x *Connector) callOllama(ctx context.Context, c protocol.Command, a AdapterConfig) error {
	body := map[string]any{"model": c.Request.Target.ModelID, "messages": c.Request.Input.Messages, "stream": true}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(a.BaseURL, "/")+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := x.modelClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return knownFailure(fmt.Sprintf("ollama returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var total int
	var done bool
	for scanner.Scan() {
		var part struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done  bool   `json:"done"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &part); err != nil {
			return fmt.Errorf("invalid Ollama stream: %w", err)
		}
		if part.Error != "" {
			return errors.New(part.Error)
		}
		if part.Message.Content != "" {
			total += len(part.Message.Content)
			if total > 10<<20 {
				return knownFailure("OUTPUT_LIMIT_EXCEEDED")
			}
			for len(part.Message.Content) > 32<<10 {
				n := 32 << 10
				for n > 0 && !utf8.RuneStart(part.Message.Content[n]) {
					n--
				}
				if err := x.emit(c, "output.delta", map[string]string{"channel": "answer", "text": part.Message.Content[:n]}); err != nil {
					return err
				}
				part.Message.Content = part.Message.Content[n:]
			}
			if err := x.emit(c, "output.delta", map[string]string{"channel": "answer", "text": part.Message.Content}); err != nil {
				return err
			}
		}
		if part.Done {
			done = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !done {
		return errors.New("Ollama stream ended without done marker")
	}
	return nil
}

func (x *Connector) emit(c protocol.Command, kind string, data any) error {
	x.mu.Lock()
	t := x.j.Tasks[c.TaskID]
	if kind == "output.delta" && !t.LeaseUntil.IsZero() && !time.Now().Before(t.LeaseUntil) {
		x.leaseStopped[c.TaskID] = true
		stop := x.running[c.TaskID]
		x.mu.Unlock()
		if stop != nil {
			stop()
		}
		return errors.New("execution lease expired")
	}
	err := x.appendSavedLocked(t, kind, data)
	x.mu.Unlock()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := x.sendPending(ctx); err != nil {
		log.Printf("event pending for replay: %v", err)
	}
	return nil
}
func (x *Connector) finish(c protocol.Command, kind string, data any) {
	x.mu.Lock()
	t := x.j.Tasks[c.TaskID]
	before := t.State
	switch kind {
	case "task.succeeded":
		t.State = "SUCCEEDED"
	case "task.failed":
		t.State = "FAILED"
	case "task.cancelled":
		t.State = "CANCELLED"
	default:
		t.State = "UNKNOWN"
	}
	t.DoneAt = time.Now().UTC()
	err := x.appendSavedLocked(t, kind, data)
	if err != nil {
		t.State = before
		t.DoneAt = time.Time{}
	}
	x.mu.Unlock()
	if err != nil {
		log.Printf("cannot persist final event: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := x.sendPending(ctx); err != nil {
		log.Printf("final event pending for replay: %v", err)
	}
}

func (x *Connector) sendPending(ctx context.Context) error {
	x.mu.Lock()
	var pending []protocol.EventBatch
	for _, t := range x.j.Tasks {
		if t.AckSeq < t.NextSeq {
			events := append([]protocol.Event(nil), t.Events...)
			pending = append(pending, protocol.EventBatch{TaskID: t.Command.TaskID, AttemptID: t.Command.AttemptID, FenceToken: t.Command.FenceToken, Events: events})
		}
	}
	x.mu.Unlock()
	for _, b := range pending {
		for len(b.Events) > 0 {
			n := len(b.Events)
			if n > 32 {
				n = 32
			}
			batch := b
			batch.Events = b.Events[:n]
			for n > 1 {
				encoded, _ := json.Marshal(batch)
				if len(encoded) < 200<<10 {
					break
				}
				n /= 2
				batch.Events = b.Events[:n]
			}
			var ack struct {
				AckSeq int64 `json:"ack_seq"`
			}
			if err := x.api(ctx, "POST", "/v1/connector/events", batch, &ack); err != nil {
				return err
			}
			x.mu.Lock()
			t := x.j.Tasks[b.TaskID]
			if t != nil && ack.AckSeq > t.AckSeq {
				if ack.AckSeq > t.NextSeq {
					x.mu.Unlock()
					return errors.New("server ACK exceeds local event sequence")
				}
				beforeAck, beforeEvents, beforeCommand := t.AckSeq, t.Events, t.Command
				t.AckSeq = ack.AckSeq
				var remaining []protocol.Event
				for _, ev := range t.Events {
					if ev.Seq > ack.AckSeq {
						remaining = append(remaining, ev)
					}
				}
				t.Events = remaining
				if isTerminal(t.State) && t.AckSeq == t.NextSeq {
					t.Command.Request.Input.Messages = nil
				}
				if err := x.saveLocked(); err != nil {
					t.AckSeq, t.Events, t.Command = beforeAck, beforeEvents, beforeCommand
					x.mu.Unlock()
					return err
				}
			}
			x.mu.Unlock()
			b.Events = b.Events[n:]
		}
	}
	return nil
}

type knownFailure string

func (e knownFailure) Error() string { return string(e) }
