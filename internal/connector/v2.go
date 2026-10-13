package connector

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"localsubproxy/internal/lsp2"
	"localsubproxy/internal/lsp2/client"
	"localsubproxy/internal/protocol"
)

// v2State holds the protocol-v2 (lsp2) session of the desktop core. In v2 the
// gateway streams results directly over the WebSocket; the v1 journal, lease
// and long-poll machinery is not used.
type v2State struct {
	mu      sync.Mutex
	client  *client.Client
	sinks   map[string]func(string) error
	active  map[string]string // task → adapter (blocks adapter edits while running)
	lastErr string
}

// DeviceName is the human-readable device name shown during pairing.
func DeviceName() string { return localDeviceName() }

// V2Models lists "adapterID/modelID" for enabled adapters that are not known to be unhealthy.
func (x *Connector) V2Models() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []string
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

type v2Executor struct{ x *Connector }

func (e v2Executor) Models() []string { return e.x.V2Models() }

func (e v2Executor) Execute(ctx context.Context, run lsp2.TaskRun, emit func(string) error) (*lsp2.TaskDone, *lsp2.ErrorBody) {
	x := e.x
	adapterID, modelID, ok := strings.Cut(run.Model, "/")
	x.mu.Lock()
	a, exists := x.adapters[adapterID]
	x.mu.Unlock()
	if !ok || !exists || !a.Enabled || !has(a.Models, modelID) {
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrForbidden, Message: "model not allowed by local adapter policy"}
	}
	op := "chat.generate"
	if a.Type == "codex_app_server" {
		op = "agent.run"
	}
	msgs := make([]protocol.Message, 0, len(run.Messages))
	for _, m := range run.Messages {
		msgs = append(msgs, protocol.Message{Role: m.Role, Content: m.Content})
	}
	c := protocol.Command{Protocol: "lsp2", Type: "task.dispatch", DeviceID: x.v2DeviceID(), TaskID: lsp2.RandomID("v2_"), AttemptID: lsp2.RandomID("attempt_"), FenceToken: 1,
		Request: protocol.CreateTask{Target: protocol.Target{DeviceID: x.v2DeviceID(), AdapterID: adapterID, ModelID: modelID}, Operation: op, Input: protocol.Input{Messages: msgs}}}
	c.Request.Options.MaxRuntimeSeconds = 600
	if err := c.Request.Validate(); err != nil {
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrBadRequest, Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, 600*time.Second)
	defer cancel()
	x.v2.mu.Lock()
	x.v2.sinks[c.TaskID], x.v2.active[c.TaskID] = emit, adapterID
	x.v2.mu.Unlock()
	x.mu.Lock()
	x.running[c.TaskID] = cancel
	x.mu.Unlock()
	defer func() {
		x.v2.mu.Lock()
		delete(x.v2.sinks, c.TaskID)
		delete(x.v2.active, c.TaskID)
		x.v2.mu.Unlock()
		x.mu.Lock()
		delete(x.running, c.TaskID)
		x.mu.Unlock()
	}()
	var err error
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
	switch {
	case err == nil:
		return &lsp2.TaskDone{FinishReason: "stop"}, nil
	case errors.Is(err, errCodexInterrupted), errors.Is(err, context.Canceled):
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrCancelled, Message: "cancelled"}
	case errors.Is(err, context.DeadlineExceeded):
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrTimeout, Message: "runtime deadline elapsed"}
	}
	var known knownFailure
	if errors.As(err, &known) && strings.HasPrefix(err.Error(), "POLICY_DENIED") {
		return nil, &lsp2.ErrorBody{Code: lsp2.ErrForbidden, Message: err.Error()}
	}
	retry := errors.As(err, &known) && (strings.HasPrefix(err.Error(), "PROVIDER_RATE_LIMITED") || strings.HasPrefix(err.Error(), "QUOTA_EXHAUSTED"))
	return nil, &lsp2.ErrorBody{Code: lsp2.ErrUpstream, Message: err.Error(), Retryable: retry}
}

func (x *Connector) v2DeviceID() string {
	if x.cfg.V2 != nil {
		return x.cfg.V2.DeviceID
	}
	return ""
}

// v2Sink returns the stream sink when a task runs over lsp2.
func (x *Connector) v2Sink(taskID string) func(string) error {
	if x.v2 == nil {
		return nil
	}
	x.v2.mu.Lock()
	defer x.v2.mu.Unlock()
	return x.v2.sinks[taskID]
}

func (x *Connector) v2AdapterActive(id string) bool {
	if x.v2 == nil {
		return false
	}
	x.v2.mu.Lock()
	defer x.v2.mu.Unlock()
	for _, a := range x.v2.active {
		if a == id {
			return true
		}
	}
	return false
}

// notifyV2Models re-announces models after adapter edits or health changes.
func (x *Connector) notifyV2Models() {
	if x.v2 == nil {
		return
	}
	x.v2.mu.Lock()
	cl := x.v2.client
	x.v2.mu.Unlock()
	if cl != nil {
		cl.ModelsChanged()
	}
}

func (x *Connector) runV2(ctx context.Context) error {
	defer x.markHeartbeat(false)
	cl, err := client.New(client.Config{Executor: v2Executor{x}, MaxConcurrency: 1, Client: "localsubproxy-desktop/lsp2",
		OnState: func(ok bool) { x.markHeartbeat(ok); x.markPoll(ok) }}, *x.cfg.V2)
	if err != nil {
		return err
	}
	x.v2.mu.Lock()
	x.v2.client = cl
	x.v2.mu.Unlock()
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
			if now := strings.Join(x.V2Models(), ","); now != last {
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
	if err != nil && ctx.Err() == nil {
		x.v2.mu.Lock()
		x.v2.lastErr = err.Error()
		x.v2.mu.Unlock()
		log.Printf("lsp2 session stopped: %v", err)
	}
	return err
}
