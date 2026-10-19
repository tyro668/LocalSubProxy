// Package connector is the desktop core: it manages local model adapters
// (Ollama, OpenAI-compatible, Anthropic-compatible, Codex app-server) and
// serves them to a paired remote service over LMBP (Local Model Bridge Protocol) as an
// lmbp client Executor.
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
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tyro668/avora/internal/audit"
	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/builtin"
	"github.com/tyro668/avora/internal/durable"
	"github.com/tyro668/avora/internal/lmbp"
	"github.com/tyro668/avora/internal/lmbp/client"
)

var backendRegistry = builtin.NewRegistry()

type Config struct {
	// RemoteURL is the paired remote service (health is shown in the UI).
	RemoteURL string
	// Identity is the paired lmbp device identity; nil means "not paired"
	// (adapters can still be managed and tested).
	Identity *client.Identity
	// DataKey is the hex AES-256 key that seals the adapter file.
	DataKey string
	// LegacyDataKeys are older keys tried when DataKey cannot open the
	// adapter file (e.g. the upstream app's journal_key); a file that opens
	// with one of them is re-encrypted with DataKey.
	LegacyDataKeys []string
	AdapterFile    string
	Adapters       []AdapterConfig
	// MaxConcurrency is the number of tasks the device accepts in parallel
	// (advertised in hello; default 4). Adapters may lower it per adapter.
	MaxConcurrency int
	// Audit, if set, receives one record per served task (no prompt content).
	Audit *audit.Log
}

// AdapterConfig and AdapterHealth are the backend types under the management API names.
type AdapterConfig = backend.Config
type AdapterHealth = backend.Health

type Connector struct {
	cfgVer          uint64            // bumped by every adapter-list change
	invalid         map[string]string // adapter id → why its saved config failed validation (never served)
	notes           map[string]string // adapter id → what the load repaired (served; shown until the scheme is edited)
	loadWarnings    []string          // saved entries that could not be loaded at all
	closed          bool              // Shutdown started: adapter changes refused
	muts            sync.WaitGroup    // adapter changes in progress (Shutdown waits)
	reqs            sync.WaitGroup    // task handling incl. early rejects and audit (Shutdown waits)
	lastSaved       []byte            // last adapters.enc content known on disk
	adaptersBlocked error             // set when adapters.enc may differ from memory
	storageWarning  string            // adapters.enc committed but not synced
	recovery        map[string]any    // set when the adapter file was unreadable and backed up
	cfg             Config
	client          *http.Client
	modelClient     *http.Client
	cipher          *durable.Cipher
	mu              sync.Mutex
	adapters        map[string]AdapterConfig
	health          map[string]AdapterHealth
	running         map[string]runSlot // linkID+"\x00"+taskID → slot
	// links are the per-SaaS lmbp sessions sharing this adapter pool.
	links    map[string]*link
	order    []string
	base     context.Context
	probing  bool
	stopProb context.CancelFunc
}

type runSlot struct{ adapter, link string }

func New(cfg Config) (*Connector, error) {
	if cfg.Identity != nil && !cfg.Identity.Valid() {
		return nil, errors.New("lmbp identity is invalid")
	}
	if cfg.RemoteURL != "" {
		if err := safeURL(cfg.RemoteURL); err != nil {
			return nil, fmt.Errorf("remote service URL: %w", err)
		}
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 4
	}
	if cfg.MaxConcurrency > lmbp.MaxConcurrency { // LMBP 2.0 §7.4: rejected, never clamped
		return nil, fmt.Errorf("max concurrency %d out of range: LMBP allows 1-%d", cfg.MaxConcurrency, lmbp.MaxConcurrency)
	}
	var c *durable.Cipher
	if cfg.DataKey != "" || cfg.AdapterFile != "" {
		var err error
		if c, err = durable.NewCipher(cfg.DataKey); err != nil {
			return nil, err
		}
	}
	x := &Connector{cfg: cfg, cipher: c, adapters: map[string]AdapterConfig{}, health: map[string]AdapterHealth{}, running: map[string]runSlot{}, links: map[string]*link{},
		client:      &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		modelClient: &http.Client{Timeout: 0, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, a := range cfg.Adapters {
		a.Name = clipBytes(strings.TrimSpace(a.Name), MaxSchemeNameBytes)
		if err := validateLoadedAdapter(&a); err != nil {
			return nil, err
		}
		if _, exists := x.adapters[a.ID]; exists {
			return nil, errors.New("duplicate adapter ID")
		}
		x.adapters[a.ID] = a
	}
	if cfg.AdapterFile != "" {
		if err := x.loadAdapterFile(); err != nil {
			return nil, err
		}
	}
	x.dedupeNamesLocked() // single-threaded: older configs may repeat names
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

// validateAdapter is the full check for a scheme being saved or tested: an
// enabled CLI scheme needs a runnable CLI.
func validateAdapter(a *AdapterConfig) error { return validateAdapterOpt(a, true) }

// validateSavedAdapter checks a scheme being saved by an edit: a disabled
// CLI scheme may keep a CLI path that does not resolve right now, so it
// can be renamed or disabled; enabling it again needs a runnable CLI.
func validateSavedAdapter(a *AdapterConfig) error { return validateAdapterOpt(a, a.Enabled) }

// validateLoadedAdapter checks a scheme read from configuration. A CLI
// that was removed, moved or lost its exec permission does not fail the
// load: the scheme stays, editable and deletable, and its health reports
// the missing CLI.
func validateLoadedAdapter(a *AdapterConfig) error { return validateAdapterOpt(a, false) }

func validateAdapterOpt(a *AdapterConfig, requireRuntime bool) error {
	if a.ID == "" || len(a.ID) > 64 || strings.Contains(a.ID, "/") || len(a.Models) == 0 || len(a.Models) > 100 {
		return errors.New("adapter ID (no '/') and 1-100 models required")
	}
	if err := checkPublishedIDs(a.ID, a.Models); err != nil {
		return err
	}
	if len(a.Name) > MaxSchemeNameBytes || len(a.Provider) > 64 || a.ContextWindow < 0 || a.ContextWindow > 2_000_000 || a.MaxOutputTokens < 0 || a.MaxOutputTokens > 1_000_000 || a.Concurrency < 0 || a.Concurrency > 16 {
		return errors.New("invalid profile settings")
	}
	for _, c := range a.Capabilities {
		if c != lmbp.ModelCapTools && c != lmbp.ModelCapVision && c != lmbp.ModelCapAudio {
			return fmt.Errorf("unknown model capability %q", c)
		}
	}
	if _, dups := lmbp.Unique(a.Capabilities); len(dups) > 0 {
		return fmt.Errorf("duplicate capability %q (each capability may be listed once)", dups[0])
	}
	return backendRegistry.Validate(a, requireRuntime)
}

// checkPublishedIDs applies the hello constraints (spec §7.4, defs.json
// modelId) to the IDs a scheme publishes, "schemeID/model": each 1..256 code
// points without whitespace, and no duplicates. A scheme that breaks them
// would make the gateway reject the whole hello or models.update.
func checkPublishedIDs(id string, models []string) error {
	if strings.IndexFunc(id, unicode.IsSpace) >= 0 {
		return fmt.Errorf("scheme ID %q contains whitespace", id)
	}
	seen := map[string]bool{}
	for _, m := range models {
		if m == "" {
			return errors.New("empty model ID")
		}
		if err := lmbp.ValidModelID(id + "/" + m); err != nil {
			return fmt.Errorf("published ID %s/…: %v", id, err)
		}
		if seen[m] {
			return fmt.Errorf("duplicate model %q", m)
		}
		seen[m] = true
	}
	return nil
}

// publishedCountLocked counts the model IDs enabled, valid schemes publish,
// leaving out scheme except (being replaced).
func (x *Connector) publishedCountLocked(except string) int {
	n := 0
	for _, a := range x.adapters {
		if a.ID != except && a.Enabled && x.invalid[a.ID] == "" {
			n += len(a.Models)
		}
	}
	return n
}

// checkTotalLocked rejects a save that would publish more than lmbp.MaxModels.
func (x *Connector) checkTotalLocked(a AdapterConfig) error {
	if !a.Enabled {
		return nil
	}
	if n := x.publishedCountLocked(a.ID) + len(a.Models); n > lmbp.MaxModels {
		return fmt.Errorf("enabled schemes would publish %d models; the protocol allows at most %d", n, lmbp.MaxModels)
	}
	// The encoded hello / models.update must fit one frame (audit3 P2-1):
	// worst case, every configured model of every enabled scheme published.
	list := []AdapterConfig{}
	for _, o := range x.adapters {
		if o.ID != a.ID && o.Enabled && x.invalid[o.ID] == "" {
			list = append(list, o)
		}
	}
	list = append(list, a)
	if n := announceBytes(list); n > announceBudget {
		return fmt.Errorf("enabled schemes would announce %d bytes of model data; one hello/models.update frame allows %d (max_frame_bytes %d less protocol overhead): use fewer or shorter model IDs and names", n, announceBudget, lmbp.MaxFrameBytes)
	}
	return nil
}

// announceBudget is the share of a hello / models.update frame the model
// list may use: max_frame_bytes less a reserve for the envelope, proof,
// e2ee key statement, flow and resume task list.
const announceBudget = lmbp.MaxFrameBytes - 16<<10

// announceOf builds the published models, capabilities and names of the
// given schemes with their published model lists.
func announceOf(as []AdapterConfig, models func(AdapterConfig) []string) ([]string, map[string][]string, map[string]string) {
	ids, caps, names := []string{}, map[string][]string{}, map[string]string{}
	for _, a := range as {
		ms := models(a)
		n := strings.TrimSpace(a.Name)
		for _, m := range ms {
			id := a.ID + "/" + m
			ids = append(ids, id)
			caps[id] = capsOf(a)
			if n == "" {
				continue
			}
			if len(a.Models) > 1 {
				names[id] = n + " · " + m
			} else {
				names[id] = n
			}
		}
	}
	return ids, caps, names
}

// announceBytes is the encoded size of the model data the schemes publish
// (every configured model).
func announceBytes(as []AdapterConfig) int {
	ids, caps, names := announceOf(as, func(a AdapterConfig) []string { return a.Models })
	b, _ := json.Marshal(lmbp.ModelsUpdate{Models: ids, ModelCapabilities: caps, ModelNames: names})
	return len(b)
}

type published struct {
	a      AdapterConfig
	models []string
}

// publishedLocked is the single source of what is announced (hello,
// models.update and its metadata): enabled, valid, healthy schemes in a
// stable order, each with the models its last probe found (audit3 P2-9;
// SkipModelProbe keeps the configured list), within lmbp.MaxModels and the
// frame budget (audit3 P2-1). Capabilities and names only describe models
// that are published.
func (x *Connector) publishedLocked() []published {
	var out []published
	total := 0
	var sel []AdapterConfig
	for _, id := range x.sortedAdapterIDsLocked() {
		a := x.adapters[id]
		if !a.Enabled || x.invalid[a.ID] != "" {
			continue
		}
		ms := a.Models
		if h, ok := x.health[a.ID]; ok {
			if h.Health != "ready" {
				continue
			}
			ms = nil
			for _, m := range a.Models { // probe results, in configured order
				if has(h.Models, m) {
					ms = append(ms, m)
				}
			}
			if len(ms) == 0 {
				continue
			}
		}
		if total+len(ms) > lmbp.MaxModels { // guarded at save and load; never send an invalid hello
			continue
		}
		p := published{a: a, models: ms}
		cand := append(append([]AdapterConfig(nil), sel...), a)
		pm := map[string][]string{}
		for _, q := range out {
			pm[q.a.ID] = q.models
		}
		pm[a.ID] = ms
		ids, caps, names := announceOf(cand, func(c AdapterConfig) []string { return pm[c.ID] })
		if b, _ := json.Marshal(lmbp.ModelsUpdate{Models: ids, ModelCapabilities: caps, ModelNames: names}); len(b) > announceBudget {
			continue // guarded at save; never send a frame the gateway closes with 1009
		}
		sel, out, total = cand, append(out, p), total+len(ms)
	}
	return out
}

// sortedAdapterIDsLocked is the stable order used to publish and to decide
// which schemes exceed the total limit.
func (x *Connector) sortedAdapterIDsLocked() []string {
	ids := make([]string, 0, len(x.adapters))
	for id := range x.adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func capsOf(a AdapterConfig) []string  { return backendRegistry.Capabilities(a) }
func adapterSlots(a AdapterConfig) int { return backendRegistry.Slots(a) }

func has(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ---------- lmbp Executor ----------

// Models lists "adapterID/modelID" for the published models (publishedLocked).
func (x *Connector) Models() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	ids, _, _ := x.announceLocked()
	return ids
}

func (x *Connector) announceLocked() ([]string, map[string][]string, map[string]string) {
	pub := x.publishedLocked()
	as := make([]AdapterConfig, len(pub))
	pm := map[string][]string{}
	for i, p := range pub {
		as[i], pm[p.a.ID] = p.a, p.models
	}
	return announceOf(as, func(a AdapterConfig) []string { return pm[a.ID] })
}

// ModelCapabilities advertises per-model flags (lmbp model_capabilities)
// for the published models only.
func (x *Connector) ModelCapabilities() map[string][]string {
	x.mu.Lock()
	defer x.mu.Unlock()
	_, caps, _ := x.announceLocked()
	return caps
}

// ModelNames advertises the scheme (adapter) name per published model id
// (lmbp model_names) so gateways can show it instead of the raw id.
func (x *Connector) ModelNames() map[string]string {
	x.mu.Lock()
	defer x.mu.Unlock()
	_, _, names := x.announceLocked()
	return names
}

// Execute runs one verified (and, for E2EE tasks, already decrypted) task and
// records it in the local audit log.
func (x *Connector) Execute(ctx context.Context, run lmbp.TaskRun, emit func(lmbp.TaskDelta) error) (*lmbp.TaskDone, *lmbp.ErrorBody) {
	return x.serve(ctx, nil, run, emit)
}

// serve runs one task for link l (nil: no connection, e.g. tests) and
// records it in the audit log attributed to that connection.
func (x *Connector) serve(ctx context.Context, l *link, run lmbp.TaskRun, emit func(lmbp.TaskDelta) error) (*lmbp.TaskDone, *lmbp.ErrorBody) {
	// Every request, also one rejected before admission, is tracked until
	// its audit row is written; once Shutdown began none starts untracked.
	x.mu.Lock()
	if x.closed {
		x.mu.Unlock()
		return nil, &lmbp.ErrorBody{Code: lmbp.ErrDeviceBusy, Message: "Avora is shutting down", Retryable: true}
	}
	x.reqs.Add(1)
	x.mu.Unlock()
	defer x.reqs.Done()
	if x.cfg.Audit == nil {
		return x.execute(ctx, l, run, emit, nil)
	}
	start := time.Now()
	var first time.Time
	// The audit row is written inside execute, before the task leaves the
	// link's inflight group, so Shutdown (and stop closing the audit DB)
	// waits for it.
	return x.execute(ctx, l, run, func(d lmbp.TaskDelta) error {
		if first.IsZero() {
			first = time.Now()
		}
		return emit(d)
	}, func(done *lmbp.TaskDone, fail *lmbp.ErrorBody) {
		x.recordAudit(l, run, start, first, done, fail)
	})
}

// auditHook runs before an audit row is written (tests).
var auditHook func()

func (x *Connector) recordAudit(l *link, run lmbp.TaskRun, start, first time.Time, done *lmbp.TaskDone, fail *lmbp.ErrorBody) {
	r := audit.Record{At: start, TaskID: run.TaskID, Model: run.Model, Status: audit.StatusOK, LatencyMS: time.Since(start).Milliseconds(),
		E2EE: run.E2EE != nil || run.Sealed, Tools: len(run.Tools) > 0}
	r.Adapter, _, _ = strings.Cut(run.Model, "/")
	if l != nil {
		id := l.cfg.Identity
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
		if fail.Code == lmbp.ErrCancelled {
			r.Status = audit.StatusCancelled
		}
	} else if done != nil && done.Usage != nil {
		r.PromptTokens, r.CompletionTokens, r.UsageReported = done.Usage.PromptTokens, done.Usage.CompletionTokens, true
	}
	if auditHook != nil {
		auditHook()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := x.cfg.Audit.Record(ctx, r); err != nil {
		log.Printf("audit: %v", err)
	}
}

func (x *Connector) execute(ctx context.Context, l *link, run lmbp.TaskRun, emit func(lmbp.TaskDelta) error, after func(*lmbp.TaskDone, *lmbp.ErrorBody)) (rdone *lmbp.TaskDone, rfail *lmbp.ErrorBody) {
	recorded := false
	record := func() {
		if after != nil && !recorded {
			recorded = true
			after(rdone, rfail)
		}
	}
	defer record() // tasks refused before admission
	adapterID, modelID, ok := strings.Cut(run.Model, "/")
	x.mu.Lock()
	a, exists := x.adapters[adapterID]
	if !ok || !exists || !a.Enabled || !has(a.Models, modelID) || x.invalid[adapterID] != "" {
		x.mu.Unlock()
		return nil, &lmbp.ErrorBody{Code: lmbp.ErrForbidden, Message: "model not allowed by local adapter policy"}
	}
	lid := ""
	if l != nil {
		lid = l.cfg.ID
		if l.closing || l.revoked || l.cfg.Paused {
			x.mu.Unlock()
			return nil, &lmbp.ErrorBody{Code: lmbp.ErrForbidden, Message: "connection is closing"}
		}
		if !l.cfg.Policy.allows(run.Model) {
			x.mu.Unlock()
			return nil, &lmbp.ErrorBody{Code: lmbp.ErrForbidden, Message: "model not allowed for this connection"}
		}
	}
	key := lid + "\x00" + run.TaskID
	if _, dup := x.running[key]; dup {
		x.mu.Unlock()
		return nil, &lmbp.ErrorBody{Code: lmbp.ErrBadRequest, Message: "duplicate task id"}
	}
	if fail := x.admitLocked(l, a); fail != nil {
		x.mu.Unlock()
		return nil, fail
	}
	x.running[key] = runSlot{adapter: adapterID, link: lid}
	if l != nil {
		l.inflight.Add(1)
		defer l.inflight.Done()
	}
	defer record() // admitted: before leaving inflight
	x.mu.Unlock()
	defer func() { x.mu.Lock(); delete(x.running, key); x.mu.Unlock() }()
	if fail := checkCaps(a, run); fail != nil {
		return nil, fail
	}
	ctx, cancel := context.WithTimeout(ctx, 600*time.Second)
	defer cancel()
	b, err := backendRegistry.Open(a, backend.Dependencies{ProbeClient: x.client, ModelClient: x.modelClient})
	if err != nil {
		return nil, backend.ErrorBody(err)
	}
	run.Model = modelID
	done, err := b.Execute(ctx, run, &backend.Emitter{Emit: emit})
	if err != nil {
		return nil, backend.ErrorBody(err)
	}
	return done, nil
}

func checkCaps(a AdapterConfig, run lmbp.TaskRun) *lmbp.ErrorBody {
	return backendRegistry.CheckRequest(a, run)
}

// ---------- session ----------

// Run serves the adapters over lmbp for the single Config.Identity until
// ctx ends or the device is revoked (CLI mode). The desktop kernel instead
// calls Start and AddLink once per paired SaaS.
func (x *Connector) Run(ctx context.Context) error {
	if x.cfg.Identity == nil {
		return errors.New("device is not paired")
	}
	if err := x.AddLink(LinkConfig{ID: "default", Identity: *x.cfg.Identity, RemoteURL: x.cfg.RemoteURL}); err != nil {
		return err
	}
	x.Start(ctx)
	x.mu.Lock()
	l := x.links["default"]
	x.mu.Unlock()
	<-l.done
	return l.err
}

// notifyModels re-announces models after adapter edits or health changes.
func (x *Connector) notifyModels() {
	x.mu.Lock()
	var cls []*client.Client
	for _, l := range x.links {
		if l.cl != nil {
			cls = append(cls, l.cl)
		}
	}
	x.mu.Unlock()
	for _, cl := range cls {
		cl.ModelsChanged()
	}
}

func (x *Connector) checkRemote(ctx context.Context, remote string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(remote, "/")+lmbp.DiscoveryPath, nil)
	if err != nil {
		return err
	}
	resp, err := x.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote service discovery: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (x *Connector) watchRemote(ctx context.Context, l *link) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := x.checkRemote(probe, l.cfg.RemoteURL)
		cancel()
		x.mu.Lock()
		l.remoteOK = err == nil
		x.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ---------- probes ----------

// CLI model discovery includes process startup and a provider request. Use the
// same budget for manual tests, post-save refreshes, and periodic health checks.
const adapterProbeTimeout = 15 * time.Second

func (x *Connector) probeAdapter(ctx context.Context, a AdapterConfig) AdapterHealth {
	if a.ID == "" {
		return AdapterHealth{}
	}
	x.mu.Lock()
	bad := x.invalid[a.ID]
	x.mu.Unlock()
	if bad != "" { // loaded broken: never probed or served until fixed
		return AdapterHealth{AdapterID: a.ID, AdapterType: a.Type, Models: a.Models, Health: "unavailable", AuthState: "unknown", Error: bad}
	}
	return x.probeConfig(ctx, a)
}

// probeConfig probes a configuration (a stored scheme or a draft under test).
func (x *Connector) probeConfig(ctx context.Context, a AdapterConfig) AdapterHealth {
	b, err := backendRegistry.Open(a, backend.Dependencies{ProbeClient: x.client, ModelClient: x.modelClient})
	if err != nil {
		return AdapterHealth{AdapterID: a.ID, AdapterType: a.Type, Health: "unavailable", Error: err.Error()}
	}
	return x.withSlots(b.Probe(ctx))
}
func (x *Connector) listProviderModels(ctx context.Context, a AdapterConfig) ([]string, error) {
	b, err := backendRegistry.Open(a, backend.Dependencies{ProbeClient: x.client, ModelClient: x.modelClient})
	if err != nil {
		return nil, err
	}
	return b.Models(ctx)
}

func (x *Connector) withSlots(h AdapterHealth) AdapterHealth {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, r := range x.running {
		if r.adapter == h.AdapterID && h.FreeSlots > 0 {
			h.FreeSlots--
		}
	}
	return h
}

func (x *Connector) probeAll(ctx context.Context) []AdapterHealth {
	ctx, cancel := context.WithTimeout(ctx, adapterProbeTimeout)
	defer cancel()
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

// loadAdapterFile reads the sealed adapter list. It tries DataKey, then
// each legacy key (re-encrypting with DataKey on success). A file that no
// key opens, or that is corrupt, must not brick startup: it is moved to
// adapters.enc.bak-<timestamp>, the list starts empty, and Status reports
// adapter_recovery so the UI can ask the user to re-add the adapters.
func (x *Connector) loadAdapterFile() error {
	f := x.cfg.AdapterFile
	x.recovery = loadRecoveryNotice(f)
	b, err := os.ReadFile(f)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	saved, migrated, openErr := openAdapters(b, x.cipher, x.cfg.LegacyDataKeys)
	if openErr == nil {
		x.lastSaved = b
	}
	if openErr != nil {
		bak := fmt.Sprintf("%s.bak-%s", f, time.Now().UTC().Format("20060102T150405Z"))
		if err := os.Rename(f, bak); err != nil {
			return fmt.Errorf("open adapters: %v; backing up the file failed: %w", openErr, err)
		}
		n := recoveryNotice{Backup: bak, Reason: openErr.Error(), At: time.Now().UTC().Format(time.RFC3339)}
		// Best effort: persist the notice so it is shown until the user
		// dismisses it, and replace the file with an empty list sealed with
		// the current key so later starts open it cleanly and never back up
		// again. Neither failure may brick startup.
		if b, err := json.Marshal(n); err == nil {
			_ = durable.Write(recoveryFile(f), b)
		}
		_ = x.saveAdaptersLocked() // single-threaded: called from New
		x.recovery = n.view()
		return nil
	}
	// A decrypted adapter that fails validation does not fail the load (the
	// file is intact, so it is never moved aside for that either): the
	// scheme stays listed, editable and deletable, is never served, and its
	// health reports why. Only an entry that cannot be addressed (bad or
	// duplicate ID) is left out, with a warning. A Codex CLI that is not
	// runnable right now is not a validation failure here (review9 P2-4).
	for _, a := range saved {
		a.Name = clipBytes(strings.TrimSpace(a.Name), MaxSchemeNameBytes) // an over-long legacy name never bricks startup
		// saved before duplicates were refused: drop repeats, keep serving
		if u, dups := lmbp.Unique(a.Capabilities); len(dups) > 0 {
			a.Capabilities = u
			if x.notes == nil {
				x.notes = map[string]string{}
			}
			x.notes[a.ID] = fmt.Sprintf("duplicate capability %q removed from the saved configuration", dups[0])
		}
		if err := validateLoadedAdapter(&a); err != nil {
			if _, dup := x.adapters[a.ID]; a.ID == "" || len(a.ID) > 64 || strings.Contains(a.ID, "/") || dup {
				x.loadWarnings = append(x.loadWarnings, fmt.Sprintf("a saved scheme with an unusable ID %q was skipped (%v); it is dropped at the next scheme change", a.ID, err))
				continue
			}
			if x.invalid == nil {
				x.invalid = map[string]string{}
			}
			x.invalid[a.ID] = "invalid configuration: " + err.Error()
		}
		x.adapters[a.ID] = a
	}
	// The total limit is checked in a stable order: the schemes that would
	// push the published list past lmbp.MaxModels become health errors.
	total := 0
	for _, id := range x.sortedAdapterIDsLocked() {
		a := x.adapters[id]
		if !a.Enabled || x.invalid[id] != "" {
			continue
		}
		if total+len(a.Models) > lmbp.MaxModels {
			if x.invalid == nil {
				x.invalid = map[string]string{}
			}
			x.invalid[id] = fmt.Sprintf("invalid configuration: enabled schemes would publish more than %d models", lmbp.MaxModels)
			continue
		}
		total += len(a.Models)
	}
	if migrated {
		return x.saveAdaptersLocked() // single-threaded: called from New
	}
	return nil
}

func openAdapters(b []byte, c *durable.Cipher, legacy []string) (saved []AdapterConfig, migrated bool, err error) {
	if err = c.Open(b, &saved); err == nil {
		return saved, false, nil
	}
	first := err
	for _, k := range legacy {
		lc, kerr := durable.NewCipher(k)
		if kerr != nil {
			continue
		}
		saved = nil
		if lc.Open(b, &saved) == nil {
			return saved, true, nil
		}
	}
	return nil, false, fmt.Errorf("adapter file cannot be decrypted with the current key: %w", first)
}

// recoveryNotice is persisted next to the adapter file after an unreadable
// file was backed up. The UI shows it once, as a dismissible notice, until
// AcknowledgeAdapterRecovery marks it acknowledged.
type recoveryNotice struct {
	Backup       string `json:"backup"`
	Reason       string `json:"reason"`
	At           string `json:"at"`
	Acknowledged bool   `json:"acknowledged"`
}

func (n recoveryNotice) view() map[string]any {
	return map[string]any{"backup": n.Backup, "reason": n.Reason, "at": n.At}
}

func recoveryFile(adapterFile string) string { return adapterFile + ".recovery.json" }

func loadRecoveryNotice(adapterFile string) map[string]any {
	var n recoveryNotice
	b, err := os.ReadFile(recoveryFile(adapterFile))
	if err != nil || json.Unmarshal(b, &n) != nil || n.Acknowledged || n.Backup == "" {
		return nil
	}
	return n.view()
}

// AcknowledgeAdapterRecovery dismisses the adapter-recovery notice for good
// (it is not reported again, also after restarts). Calling it when there is
// no notice is a no-op.
func (x *Connector) AcknowledgeAdapterRecovery() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.recovery = nil
	if x.cfg.AdapterFile == "" {
		return nil
	}
	f := recoveryFile(x.cfg.AdapterFile)
	b, err := os.ReadFile(f)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var n recoveryNotice
	_ = json.Unmarshal(b, &n)
	n.Acknowledged = true
	if b, err = json.Marshal(n); err != nil {
		return err
	}
	return durable.Write(f, b)
}
