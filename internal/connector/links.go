package connector

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
)

// Policy is the per-connection (per-SaaS) local policy.
type Policy struct {
	// Allow lists the models this SaaS may see and call: "adapter/model",
	// "adapter/*" or "*". Empty denies everything (default for new pairings).
	Allow []string `json:"allow"`
	// MaxConcurrency caps this connection's parallel tasks (0: the global cap).
	MaxConcurrency int `json:"max_concurrency,omitempty"`
}

// allows reports whether model ("adapter/model") is permitted. A nil policy
// (single-connection CLI mode) allows every local model.
func (p *Policy) allows(model string) bool {
	if p == nil {
		return true
	}
	a, _, _ := strings.Cut(model, "/")
	for _, e := range p.Allow {
		if e == "*" || e == model || e == a+"/*" {
			return true
		}
	}
	return false
}

func (p *Policy) clone() *Policy {
	if p == nil {
		return nil
	}
	return &Policy{Allow: append([]string{}, p.Allow...), MaxConcurrency: p.MaxConcurrency}
}

func validatePolicy(p *Policy) error {
	if p == nil {
		return nil
	}
	if len(p.Allow) > 500 || p.MaxConcurrency < 0 || p.MaxConcurrency > 64 {
		return errors.New("invalid connection policy")
	}
	for _, e := range p.Allow {
		if e == "" || len(e) > 200 || (e != "*" && !strings.Contains(e, "/")) {
			return fmt.Errorf("invalid allow entry %q", e)
		}
	}
	return nil
}

// LinkConfig is one paired SaaS gateway.
type LinkConfig struct {
	ID        string // local binding id (device ids may repeat across gateways)
	Identity  client.Identity
	RemoteURL string
	Policy    *Policy
	Paused    bool // keep the pairing but do not connect
}

type link struct {
	cfg                 LinkConfig
	cl                  *client.Client
	remoteOK, gatewayOK bool
	revoked             bool
	closing             bool           // being removed: no new tasks
	inflight            sync.WaitGroup // tasks admitted on this link
	lastErr             string
	cancel              context.CancelFunc
	done                chan struct{}
	err                 error
}

func (l *link) active() bool { return !l.cfg.Paused && !l.revoked && !l.closing }

// linkExec is the client.Executor for one connection: it filters the model
// list by the connection policy and attributes tasks to the connection.
type linkExec struct {
	x *Connector
	l *link
}

func (e linkExec) policy() *Policy {
	e.x.mu.Lock()
	defer e.x.mu.Unlock()
	return e.l.cfg.Policy.clone()
}

func (e linkExec) Models() []string {
	p := e.policy()
	out := []string{}
	for _, m := range e.x.Models() {
		if p.allows(m) {
			out = append(out, m)
		}
	}
	return out
}

func (e linkExec) ModelCapabilities() map[string][]string {
	p := e.policy()
	out := map[string][]string{}
	for m, c := range e.x.ModelCapabilities() {
		if p.allows(m) {
			out[m] = c
		}
	}
	return out
}

func (e linkExec) Execute(ctx context.Context, run lmbp.TaskRun, emit func(lmbp.TaskDelta) error) (*lmbp.TaskDone, *lmbp.ErrorBody) {
	return e.x.serve(ctx, e.l, run, emit)
}

func (x *Connector) linkCapLocked(l *link) int {
	c := x.cfg.MaxConcurrency
	if l != nil && l.cfg.Policy != nil && l.cfg.Policy.MaxConcurrency > 0 && l.cfg.Policy.MaxConcurrency < c {
		c = l.cfg.Policy.MaxConcurrency
	}
	return c
}

// admitLocked applies the adapter, global, per-connection and fair-share
// limits. Fair share: with n active connections each is guaranteed
// max(1, global/n) slots; a connection may exceed its share only while the
// slots still owed to the other connections remain free.
func (x *Connector) admitLocked(l *link, a AdapterConfig) *lmbp.ErrorBody {
	busy := func(msg string) *lmbp.ErrorBody {
		return &lmbp.ErrorBody{Code: lmbp.ErrDeviceBusy, Message: msg, Retryable: true}
	}
	perAdapter, perLink := 0, map[string]int{}
	for _, r := range x.running {
		if r.adapter == a.ID {
			perAdapter++
		}
		perLink[r.link]++
	}
	if perAdapter >= adapterSlots(a) {
		return busy("adapter is at its concurrency limit")
	}
	global := x.cfg.MaxConcurrency
	if len(x.running) >= global {
		return busy("device is at its concurrency limit")
	}
	if l == nil {
		return nil
	}
	mine := perLink[l.cfg.ID]
	if mine >= x.linkCapLocked(l) {
		return busy("connection is at its concurrency limit")
	}
	active := 0
	for _, o := range x.links {
		if o.active() {
			active++
		}
	}
	if active < 1 {
		active = 1
	}
	share := global / active
	if share < 1 {
		share = 1
	}
	if mine < share {
		return nil
	}
	owed := 0
	for id, o := range x.links {
		if id == l.cfg.ID || !o.active() {
			continue
		}
		want := share
		if c := x.linkCapLocked(o); c < want {
			want = c
		}
		if n := want - perLink[id]; n > 0 {
			owed += n
		}
	}
	if len(x.running)+owed >= global {
		return busy("connection is above its fair share while others wait")
	}
	return nil
}

// Health loop knobs; tests shorten or drive them.
var (
	healthInterval = 25 * time.Second
	healthKick     chan struct{} // nil: never fires
	healthRound    func()
	healthBefore   func()
)

// Start begins adapter health probing and connects every active link added
// so far; links added later connect immediately. ctx bounds all sessions.
func (x *Connector) Start(ctx context.Context) {
	x.mu.Lock()
	if x.base != nil {
		x.mu.Unlock()
		return
	}
	x.base = ctx
	var start []*link
	for _, id := range x.order {
		if l := x.links[id]; l.active() {
			start = append(start, l)
		}
	}
	x.mu.Unlock()
	interval, kick, roundDone, before := healthInterval, healthKick, healthRound, healthBefore
	go func() { // keep adapter health fresh and re-announce when the model list changes
		// A hello may have gone out before the first probe finished (with
		// the unprobed list), so the first probe result is always
		// announced; after that every change notifies, including
		// all-offline (empty) and the recovery from empty to non-empty.
		first, last := true, ""
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if before != nil {
				before()
			}
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			x.probeAll(pctx)
			cancel()
			if now := strings.Join(x.Models(), ","); first || now != last {
				x.notifyModels() // a no-op re-announce before any session is live
				first, last = false, now
			}
			if roundDone != nil {
				roundDone()
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-kick:
			}
		}
	}()
	for _, l := range start {
		x.launch(l)
	}
}

// AddLink registers one paired SaaS. Two links may not share the same
// gateway + tenant (one binding per SaaS).
func (x *Connector) AddLink(c LinkConfig) error {
	if c.ID == "" || len(c.ID) > 64 || strings.ContainsAny(c.ID, "/\x00") {
		return errors.New("invalid connection id")
	}
	if !c.Identity.Valid() {
		return errors.New("lmbp identity is invalid")
	}
	if c.RemoteURL != "" {
		if err := safeURL(c.RemoteURL); err != nil {
			return fmt.Errorf("remote service URL: %w", err)
		}
	}
	if err := validatePolicy(c.Policy); err != nil {
		return err
	}
	c.Policy = c.Policy.clone()
	if strings.TrimRight(c.RemoteURL, "/") == c.Identity.Issuer {
		c.RemoteURL = "" // the gateway itself: its session state is the health signal
	}
	x.mu.Lock()
	if _, dup := x.links[c.ID]; dup {
		x.mu.Unlock()
		return errors.New("duplicate connection id")
	}
	for _, o := range x.links {
		if o.cfg.Identity.Issuer == c.Identity.Issuer && o.cfg.Identity.Tenant == c.Identity.Tenant {
			x.mu.Unlock()
			return errors.New("already connected to this SaaS (same gateway and tenant)")
		}
	}
	l := &link{cfg: c, remoteOK: c.RemoteURL == "", done: closedChan()}
	x.links[c.ID] = l
	x.order = append(x.order, c.ID)
	started := x.base != nil
	x.mu.Unlock()
	if started && l.active() {
		x.launch(l)
	}
	return nil
}

func closedChan() chan struct{} { c := make(chan struct{}); close(c); return c }

func (x *Connector) launch(l *link) {
	x.mu.Lock()
	if x.base == nil || l.cancel != nil {
		x.mu.Unlock()
		return
	}
	cl, err := client.New(client.Config{Executor: linkExec{x, l}, MaxConcurrency: x.linkCapLocked(l), Client: "localsubproxy-desktop/lmbp",
		OnState: func(ok bool) { x.mu.Lock(); l.gatewayOK = ok; x.mu.Unlock() }}, l.cfg.Identity)
	if err != nil {
		l.lastErr, l.err = err.Error(), err
		x.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(x.base)
	l.cl, l.cancel, l.done, l.lastErr, l.revoked = cl, cancel, make(chan struct{}), "", false
	done := l.done
	x.mu.Unlock()
	if l.cfg.RemoteURL != "" {
		go x.watchRemote(ctx, l)
	}
	go func() {
		err := cl.Run(ctx, nil)
		x.mu.Lock()
		l.gatewayOK, l.err = false, err
		if err != nil && ctx.Err() == nil {
			l.lastErr = err.Error()
			if errors.Is(err, client.ErrRevoked) {
				l.revoked = true
			}
			log.Printf("lmbp session %s stopped: %v", l.cfg.ID, err)
		}
		if l.cancel != nil {
			l.cancel()
		}
		l.cancel, l.cl = nil, nil
		x.mu.Unlock()
		// Run aborted the tasks on a permanent stop (revoke or halt); wait
		// until they released their adapter and concurrency slots.
		l.inflight.Wait()
		close(done)
	}()
}

// halt stops a link's session and waits for it.
func (x *Connector) halt(l *link) {
	x.mu.Lock()
	cancel, done := l.cancel, l.done
	x.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	<-done
}

func (x *Connector) linkRunningLocked(id string) int {
	n := 0
	for _, r := range x.running {
		if r.link == id {
			n++
		}
	}
	return n
}

func (x *Connector) getLink(id string) (*link, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	l, ok := x.links[id]
	if !ok {
		return nil, errors.New("unknown connection")
	}
	return l, nil
}

// BeginRemove marks a connection as closing: it must have no running
// tasks, and no new task is admitted afterwards. Follow with FinishRemove
// (after the removal was persisted) or AbortRemove.
func (x *Connector) BeginRemove(id string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	l, ok := x.links[id]
	if !ok {
		return errors.New("unknown connection")
	}
	if l.closing {
		return nil // retry after an uncertain save: still closing
	}
	if x.linkRunningLocked(id) > 0 {
		return errors.New("tasks are still running on this connection")
	}
	l.closing = true
	return nil
}

// HasLink reports whether the connection is loaded in this core (it may
// be serving even if the kernel failed to apply a state change to it).
func (x *Connector) HasLink(id string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	_, ok := x.links[id]
	return ok
}

// Fence makes a connection refuse new tasks at once (running ones go on).
// Used when the kernel could not apply a saved state change to it.
func (x *Connector) Fence(id string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if l, ok := x.links[id]; ok {
		l.closing = true
	}
}

// ExecuteOn runs one task on a connection exactly as a gateway task.run
// would be served (policy, admission, audit).
func (x *Connector) ExecuteOn(ctx context.Context, id string, run lmbp.TaskRun, emit func(lmbp.TaskDelta) error) (*lmbp.TaskDone, *lmbp.ErrorBody) {
	l, err := x.getLink(id)
	if err != nil {
		return nil, &lmbp.ErrorBody{Code: lmbp.ErrBadRequest, Message: err.Error()}
	}
	return x.serve(ctx, l, run, emit)
}

// Closing reports whether a connection refuses new tasks (BeginRemove).
func (x *Connector) Closing(id string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	l, ok := x.links[id]
	return ok && l.closing
}

// AbortRemove undoes BeginRemove.
func (x *Connector) AbortRemove(id string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if l, ok := x.links[id]; ok {
		l.closing = false
	}
}

// FinishRemove stops the session, waits for task cleanup and forgets the link.
func (x *Connector) FinishRemove(id string) {
	l, err := x.getLink(id)
	if err != nil {
		return
	}
	x.halt(l)
	l.inflight.Wait()
	x.mu.Lock()
	delete(x.links, id)
	for i, o := range x.order {
		if o == id {
			x.order = append(x.order[:i:i], x.order[i+1:]...)
			break
		}
	}
	x.mu.Unlock()
}

// RemoveLink disconnects one SaaS and forgets it. Other links are untouched.
func (x *Connector) RemoveLink(id string) error {
	if err := x.BeginRemove(id); err != nil {
		return err
	}
	x.FinishRemove(id)
	return nil
}

// SetPaused disconnects (true) or reconnects (false) one SaaS, keeping its
// pairing. Pausing is refused while tasks run (they could not resume).
func (x *Connector) SetPaused(id string, paused bool) error {
	l, err := x.getLink(id)
	if err != nil {
		return err
	}
	x.mu.Lock()
	if paused && x.linkRunningLocked(id) > 0 {
		x.mu.Unlock()
		return errors.New("tasks are still running on this connection")
	}
	l.cfg.Paused = paused
	x.mu.Unlock()
	if paused {
		x.halt(l)
		return nil
	}
	x.launch(l)
	return nil
}

// SetPolicy replaces one connection's policy. The allow-list applies to the
// next task; a changed concurrency cap is applied to the live session at
// once (models.update) and to every later reconnect, even while tasks run.
func (x *Connector) SetPolicy(id string, p Policy) error {
	if err := validatePolicy(&p); err != nil {
		return err
	}
	l, err := x.getLink(id)
	if err != nil {
		return err
	}
	x.mu.Lock()
	l.cfg.Policy = p.clone()
	cl, capNow := l.cl, x.linkCapLocked(l)
	x.mu.Unlock()
	if cl != nil {
		cl.SetMaxConcurrency(capNow) // also re-announces the filtered models
	}
	return nil
}

// Policy returns a copy of one connection's policy.
func (x *Connector) Policy(id string) (Policy, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	l, ok := x.links[id]
	if !ok {
		return Policy{}, errors.New("unknown connection")
	}
	if l.cfg.Policy == nil {
		return Policy{Allow: []string{"*"}}, nil
	}
	return *l.cfg.Policy.clone(), nil
}

// Shutdown stops every session.
func (x *Connector) Shutdown() {
	// refuse new adapter changes and wait for running ones, so none
	// writes adapters.enc after a newer core started on the same file
	x.mu.Lock()
	x.closed = true
	x.mu.Unlock()
	x.muts.Wait()
	x.mu.Lock()
	ls := make([]*link, 0, len(x.links))
	for _, l := range x.links {
		ls = append(ls, l)
	}
	x.mu.Unlock()
	for _, l := range ls {
		x.halt(l)
		l.inflight.Wait()
	}
	x.reqs.Wait() // early rejects and their audit rows too
}

// ConnectionIDs lists connection ids in order.
func (x *Connector) ConnectionIDs() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]string{}, x.order...)
}

// ConnectionRunning is the number of tasks running on one connection.
func (x *Connector) ConnectionRunning(id string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.linkRunningLocked(id)
}

func (x *Connector) linkStatusLocked(l *link) map[string]any {
	id := l.cfg.Identity
	transport := "wss"
	if strings.HasPrefix(id.GatewayURL, "http://") {
		transport = "ws"
	}
	state := "connecting"
	switch {
	case l.revoked:
		state = "revoked"
	case l.cfg.Paused:
		state = "disconnected"
	case l.remoteOK && l.gatewayOK:
		state = "online"
	case l.cancel == nil && l.lastErr != "":
		state = "error"
	}
	allow := []string{}
	maxc := x.linkCapLocked(l)
	if l.cfg.Policy != nil {
		allow = append(allow, l.cfg.Policy.Allow...)
	} else {
		allow = append(allow, "*")
	}
	return map[string]any{"id": l.cfg.ID, "saas_name": id.SaaSName, "issuer": id.Issuer, "tenant": id.Tenant, "device_id": id.DeviceID,
		"remote_url": l.cfg.RemoteURL, "server_url": id.GatewayURL, "state": state, "connected": l.remoteOK && l.gatewayOK,
		"remote_connected": l.remoteOK, "gateway_connected": l.gatewayOK, "transport": transport, "e2ee": id.E2EEKey != "",
		"running_tasks": x.linkRunningLocked(l.cfg.ID), "max_concurrency": maxc, "allow": allow, "paused": l.cfg.Paused, "last_error": l.lastErr}
}

// ValidatePolicy checks a policy before it is stored.
func (x *Connector) ValidatePolicy(p Policy) error { return validatePolicy(&p) }
