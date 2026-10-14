package gateway

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// event is one device→gateway data frame (or a synthetic gateway terminal
// event) as exposed to SSE subscribers; Seq doubles as the SSE event id.
type event struct {
	Seq  int64
	Type string
	Data json.RawMessage
}

// gtask is the gateway-side hub of one task: it buffers the device's frames,
// fans them out to (re-)attached HTTP subscribers and paces task.ack so a
// slow subscriber exerts backpressure on the device (docs/lmbp-spec.md §8).
type gtask struct {
	g                 *Gateway
	id, tenant, owner string
	reqModel          string
	created           int64
	e2ee, idempotent  bool
	ds                *dstate
	sentRun           atomic.Bool
	ctx               context.Context
	stop              context.CancelFunc
	mu                sync.Mutex
	events            []event
	notify            chan struct{}
	done              bool
	subs              map[int]int64 // subscriber → delivered seq
	nextSub           int
	acked             int64
	persisted         int64 // highest device seq durably stored; never ACK beyond it
	ackAt             time.Time
	grace             *time.Timer
	slot              bool
}

func (g *Gateway) newTask(ds *dstate, id, tenant, owner, reqModel string, e2ee, idem bool) *gtask {
	ctx, stop := context.WithCancel(context.Background())
	t := &gtask{g: g, id: id, tenant: tenant, owner: owner, reqModel: reqModel, created: time.Now().Unix(), e2ee: e2ee, idempotent: idem,
		ds: ds, ctx: ctx, stop: stop, notify: make(chan struct{}), subs: map[int]int64{}, ackAt: time.Now()}
	g.mu.Lock()
	g.tasks[id] = t
	g.mu.Unlock()
	if ds != nil {
		ds.mu.Lock()
		ds.tasks[id] = t
		ds.mu.Unlock()
		time.AfterFunc(g.cfg.TaskTimeout, func() {
			if t.cancel("timeout") {
				t.failLocal(lmbp.ErrTimeout, "task timed out", false)
			}
		})
		go t.watchdog()
	}
	return t
}

// abandon drops a hub that never became a task (lost the idempotency race
// or the record could not be written); nobody can be attached to it yet.
func (t *gtask) abandon() {
	t.mu.Lock()
	t.done = true
	t.mu.Unlock()
	t.g.mu.Lock()
	if t.g.tasks[t.id] == t {
		delete(t.g.tasks, t.id)
	}
	t.g.mu.Unlock()
	if t.ds != nil {
		t.ds.mu.Lock()
		delete(t.ds.tasks, t.id)
		t.ds.mu.Unlock()
	}
	t.stop()
}

func (t *gtask) isDone() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// finishedTask rebuilds a read-only finished task from storage (re-attach
// after the in-memory hub was dropped).
func (g *Gateway) finishedTask(st store.Task, reqModel string) (*gtask, error) {
	evs, err := g.cfg.Store.ListEvents(context.Background(), st.ID)
	if err != nil {
		return nil, err
	}
	t := &gtask{g: g, id: st.ID, tenant: st.TenantID, owner: st.OwnerID, reqModel: reqModel, created: st.CreatedAt.Unix(), notify: make(chan struct{}), subs: map[int]int64{}, done: true}
	for _, e := range evs {
		t.events = append(t.events, event{Seq: e.Seq, Type: e.Kind, Data: e.Data})
	}
	// Recovery decides from a consistent terminal state: the last event is
	// terminal AND the stored state agrees with it (FinishTask writes both
	// atomically). Anything else (no terminal event, or a legacy row whose
	// event and state disagree) is reported as OUTCOME_UNKNOWN. That event is
	// synthetic, only served, never stored, and takes the next unused seq.
	var lastSeq int64
	if n := len(t.events); n > 0 {
		lastSeq = t.events[n-1].Seq
	}
	consistent := false
	if n := len(t.events); n > 0 {
		switch t.events[n-1].Type {
		case lmbp.TypeTaskDone:
			consistent = st.State == "SUCCEEDED"
		case lmbp.TypeTaskError:
			consistent = st.State == "FAILED" || st.State == "CANCELLED"
		}
		if !consistent && isTerminal(t.events[n-1].Type) {
			g.cfg.Logger.Printf("lmbp: task %s: stored terminal %s disagrees with state %s; reporting %s", st.ID, t.events[n-1].Type, st.State, lmbp.ErrOutcomeUnknown)
			t.events = t.events[:n-1]
		}
	}
	if !consistent {
		b, _ := json.Marshal(lmbp.ErrorBody{Code: lmbp.ErrOutcomeUnknown, Message: "task outcome unknown"})
		t.events = append(t.events, event{Seq: lastSeq + 1, Type: lmbp.TypeTaskError, Data: b})
	}
	return t, nil
}

func isTerminal(typ string) bool { return typ == lmbp.TypeTaskDone || typ == lmbp.TypeTaskError }

func (t *gtask) recvSeq() int64 {
	t.ds.mu.Lock()
	in := t.ds.in
	t.ds.mu.Unlock()
	if in == nil {
		return 0
	}
	return in.Recv(t.id)
}

func (t *gtask) broadcastLocked() {
	close(t.notify)
	t.notify = make(chan struct{})
}

// onDeviceFrame records an accepted, in-order device data frame.
func (t *gtask) onDeviceFrame(in lmbp.Envelope) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		t.maybeAck()
		return
	}
	data := in.Data
	if in.Type == lmbp.TypeTaskDelta && !t.e2ee {
		// Drop any e2ee field the device must not send for a cleartext task.
		var d lmbp.TaskDelta
		if json.Unmarshal(data, &d) == nil && d.E2EE != "" {
			d.E2EE = ""
			data, _ = json.Marshal(d)
		}
	}
	t.mu.Unlock()
	// Persist before publishing or ACKing: an ACK tells the device it may
	// drop the frame from its resume buffer, so it must only cover frames the
	// gateway can still serve after a restart (re-attach / replay).
	ctx := context.Background()
	ev := store.Event{TaskID: t.id, Seq: in.Seq, Kind: in.Type, Data: data, At: time.Now()}
	state := ""
	if isTerminal(in.Type) {
		state = "SUCCEEDED"
		if in.Type == lmbp.TypeTaskError {
			state = "FAILED"
			var eb lmbp.ErrorBody
			if json.Unmarshal(data, &eb) == nil && eb.Code == lmbp.ErrCancelled {
				state = "CANCELLED"
			}
		}
		// Terminal event + terminal state in one transaction: if it fails,
		// neither is stored, so the local INTERNAL error below can take this
		// seq without conflicting with a half-written done.
		if err := t.g.cfg.Store.FinishTask(ctx, ev, state); err != nil {
			t.persistFailed(in.Seq, "terminal event and state", err)
			return
		}
	} else if err := t.g.cfg.Store.AppendEvent(ctx, ev); err != nil {
		t.persistFailed(in.Seq, "event", err)
		return
	}
	t.mu.Lock()
	if t.done { // failed locally while we were writing
		t.mu.Unlock()
		return
	}
	t.events = append(t.events, event{Seq: in.Seq, Type: in.Type, Data: data})
	t.persisted = in.Seq
	t.broadcastLocked()
	t.mu.Unlock()
	if state != "" {
		t.finishWith(state, true, false)
		return
	}
	t.maybeAck()
}

// persistFailed handles a failed write of device output: the frame is not
// ACKed (the device keeps it), the device is told to stop, and every caller
// gets INTERNAL instead of a result the gateway could not make durable.
func (t *gtask) persistFailed(seq int64, what string, err error) {
	t.g.cfg.Logger.Printf("lmbp: task %s: persisting %s for seq %d failed: %v (not acknowledged)", t.id, what, seq, err)
	t.cancel("internal_error")
	t.failLocal(lmbp.ErrInternal, "gateway could not persist task output", false)
}

// maybeAck acknowledges what every attached subscriber has consumed (or all
// received frames when nobody is attached; they are buffered for re-attach).
func (t *gtask) maybeAck() {
	if t.ds == nil {
		return
	}
	recv := t.recvSeq()
	t.mu.Lock()
	target := min(recv, t.persisted)
	if !t.done {
		for _, d := range t.subs {
			if d < target {
				target = d
			}
		}
	}
	if target <= t.acked {
		t.mu.Unlock()
		return
	}
	t.acked, t.ackAt = target, time.Now()
	t.mu.Unlock()
	t.ds.mu.Lock()
	in := t.ds.in
	t.ds.mu.Unlock()
	if in != nil {
		in.Acked(t.id, target)
	}
	b, _ := json.Marshal(lmbp.TaskAck{Seq: target})
	_ = t.ds.control(lmbp.Envelope{Type: lmbp.TypeTaskAck, TaskID: t.id, Data: b})
}

// watchdog fails the task when subscribers stop consuming for too long.
func (t *gtask) watchdog() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-tick.C:
			recv := t.recvSeq()
			t.mu.Lock()
			stalled := len(t.subs) > 0 && recv > t.acked && time.Since(t.ackAt) > t.g.cfg.SlowConsumer
			t.mu.Unlock()
			if stalled && t.cancel("slow_consumer") {
				t.failLocal(lmbp.ErrSlowConsumer, "client did not consume the stream in time", false)
			}
		}
	}
}

// cancel tells the device to stop; false if the task already finished.
func (t *gtask) cancel(reason string) bool {
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()
	if done {
		return false
	}
	if t.ds != nil && t.sentRun.Load() {
		b, _ := json.Marshal(lmbp.TaskCancel{Reason: reason})
		_ = t.ds.control(lmbp.Envelope{Type: lmbp.TypeTaskCancel, TaskID: t.id, Data: b})
	}
	return true
}

// failLocal appends a synthetic gateway terminal error event.
func (t *gtask) failLocal(code, msg string, retryable bool) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	seq := int64(1)
	if n := len(t.events); n > 0 {
		seq = t.events[n-1].Seq + 1
	}
	b, _ := json.Marshal(lmbp.ErrorBody{Code: code, Message: msg, Retryable: retryable})
	t.events = append(t.events, event{Seq: seq, Type: lmbp.TypeTaskError, Data: b})
	t.broadcastLocked()
	t.mu.Unlock()
	state := "FAILED"
	if code == lmbp.ErrCancelled {
		state = "CANCELLED"
	}
	// A gateway-local terminal event is not a device frame (nothing to ACK).
	// It is stored atomically with the terminal state; if that fails nothing
	// is stored and a later re-attach reports OUTCOME_UNKNOWN (an error, never
	// a success).
	if err := t.g.cfg.Store.FinishTask(context.Background(), store.Event{TaskID: t.id, Seq: seq, Kind: lmbp.TypeTaskError, Data: b, At: time.Now()}, state); err != nil && err != store.ErrNotFound {
		t.g.cfg.Logger.Printf("lmbp: task %s: persisting local %s failed: %v", t.id, code, err)
	}
	t.finishWith(state, false, false)
}

// failOffline ends a task whose device is gone: DEVICE_OFFLINE is retryable
// only if task.run was never written (at-most-once execution).
func (t *gtask) failOffline(code, msg string) {
	if code == lmbp.ErrDeviceOffline && t.sentRun.Load() {
		code, msg = lmbp.ErrOutcomeUnknown, msg+"; outcome unknown"
	}
	t.failLocal(code, msg, code == lmbp.ErrDeviceOffline)
}

// finishWith ends the in-memory task. The terminal state is always stored by
// the caller together with its terminal event (Store.FinishTask).
func (t *gtask) finishWith(state string, fromDevice, _ bool) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	t.done = true
	slot := t.slot
	t.slot = false
	if t.grace != nil {
		t.grace.Stop()
	}
	t.broadcastLocked()
	t.mu.Unlock()
	if ds := t.ds; ds != nil {
		if fromDevice {
			t.maybeAck() // release the device's resume buffer
		}
		ds.mu.Lock()
		delete(ds.tasks, t.id)
		in := ds.in
		ds.mu.Unlock()
		ds.out.Forget(t.id)
		if in != nil {
			in.Forget(t.id)
		}
		if slot {
			ds.release()
		}
	}
	t.stop()
	time.AfterFunc(t.g.cfg.AttachGrace+10*time.Minute, func() {
		t.g.mu.Lock()
		if t.g.tasks[t.id] == t {
			delete(t.g.tasks, t.id)
		}
		t.g.mu.Unlock()
	})
}

// attach registers a subscriber that has already seen events <= after.
func (t *gtask) attach(after int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextSub++
	t.subs[t.nextSub] = after
	if t.grace != nil {
		t.grace.Stop()
		t.grace = nil
	}
	return t.nextSub
}

// detach removes a subscriber. When the last one leaves an unfinished task,
// an idempotent task gets AttachGrace to be re-attached; others are cancelled.
func (t *gtask) detach(id int) {
	t.mu.Lock()
	delete(t.subs, id)
	left := len(t.subs) == 0 && !t.done
	if left && t.idempotent {
		t.grace = time.AfterFunc(t.g.cfg.AttachGrace, func() {
			t.mu.Lock()
			still := len(t.subs) == 0
			t.mu.Unlock()
			if still && t.cancel("client_disconnected") {
				t.failLocal(lmbp.ErrCancelled, "client did not re-attach", false)
			}
		})
	}
	t.mu.Unlock()
	if left && !t.idempotent {
		if t.cancel("client_disconnected") {
			t.failLocal(lmbp.ErrCancelled, "client disconnected", false)
		}
		return
	}
	t.maybeAck()
}

// stream delivers events with seq > after to fn, in order, until the
// terminal event or ctx ends. Each delivered event advances the ack point.
func (t *gtask) stream(ctx context.Context, after int64, keepalive func() error, fn func(event) error) error {
	sub := t.attach(after)
	defer t.detach(sub)
	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	for {
		t.mu.Lock()
		var batch []event
		for _, e := range t.events {
			if e.Seq > after {
				batch = append(batch, e)
			}
		}
		wait := t.notify
		// Finished task and nothing after the cursor (Last-Event-ID equal to
		// or beyond the terminal seq): end instead of waiting forever.
		ended := len(batch) == 0 && len(t.events) > 0 && isTerminal(t.events[len(t.events)-1].Type)
		t.mu.Unlock()
		if ended {
			return nil
		}
		for _, e := range batch {
			if err := fn(e); err != nil {
				return err
			}
			after = e.Seq
			t.mu.Lock()
			t.subs[sub] = after
			t.mu.Unlock()
			if isTerminal(e.Type) {
				return nil
			}
		}
		t.maybeAck()
		if len(batch) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		case <-keep.C:
			if keepalive != nil {
				if err := keepalive(); err != nil {
					return err
				}
			}
		}
	}
}
