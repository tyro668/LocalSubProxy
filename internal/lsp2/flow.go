package lsp2

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// Reliable, flow-controlled delivery of data frames (docs/protocol-v2.md §8).
//
// Each side numbers its data frames per task (seq 1, 2, ...). A frame stays
// "unacknowledged" until the peer sends task.ack{seq>=n}. The peer advertises
// two receive windows (Flow): the sender MUST keep the total size (bytes of
// the serialized frame) of unacknowledged frames <= task_window per task and
// <= conn_window across all tasks. Unacknowledged frames are exactly what is
// retransmitted after a resume, so the windows also bound resume buffers.

var ErrOutboxClosed = errors.New("outbox closed")

type outFrame struct {
	seq int64
	raw []byte
}

type outTask struct {
	nextSeq int64
	unacked []outFrame
	used    int
	// written is the highest seq handed to the current connection; -1 means
	// "paused": the connection was (re)attached and the task waits for
	// Retransmit, so live sends cannot overtake retransmitted frames.
	written int64
}

type waiter struct {
	task string
	n    int
	ch   chan struct{}
}

// Outbox is the sending half. It is safe for concurrent use; Send blocks
// while the windows are full and admits blocked tasks in FIFO order, which
// gives round-robin fairness between concurrently streaming tasks.
type Outbox struct {
	mu       sync.Mutex
	flow     *Flow // peer receive windows; nil = unlimited
	connUsed int
	tasks    map[string]*outTask
	waiters  []*waiter
	write    func([]byte) error
	wmu      sync.Mutex // serializes flush so frames go out in seq order
	closed   bool
}

func NewOutbox() *Outbox { return &Outbox{tasks: map[string]*outTask{}} }

// SetFlow installs the peer's advertised windows (nil disables flow control).
func (o *Outbox) SetFlow(f *Flow) {
	o.mu.Lock()
	o.flow = f
	o.wakeLocked()
	o.mu.Unlock()
}

// Attach sets the live connection writer (nil when disconnected).
//
// Existing tasks are paused on attach until Retransmit is called for them,
// so a resumed stream is always delivered gap-free and in order.
func (o *Outbox) Attach(write func([]byte) error) {
	o.mu.Lock()
	o.write = write
	for _, t := range o.tasks {
		t.written = -1
	}
	o.mu.Unlock()
}

// flush writes, in seq order, the task's frames that the current connection
// has not been given yet. Writing happens outside o.mu so acks are never
// blocked behind a slow socket.
func (o *Outbox) flush(taskID string) error {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	for {
		o.mu.Lock()
		t := o.tasks[taskID]
		w := o.write
		if t == nil || w == nil || t.written < 0 {
			o.mu.Unlock()
			return nil
		}
		var raw []byte
		for _, f := range t.unacked {
			if f.seq > t.written {
				raw, t.written = f.raw, f.seq
				break
			}
		}
		o.mu.Unlock()
		if raw == nil {
			return nil
		}
		if err := w(raw); err != nil {
			return err // the frame stays unacked; resume retransmits it
		}
	}
}

func (o *Outbox) task(id string) *outTask {
	t := o.tasks[id]
	if t == nil {
		t = &outTask{}
		o.tasks[id] = t
	}
	return t
}

func (o *Outbox) fitsLocked(id string, n int) bool {
	return CanSend(o.flow, o.task(id).used, o.connUsed, n)
}

// CanSend is the normative sender rule (docs/protocol-v2.md §15.3): a data
// frame of n bytes may be sent when it fits both windows. A frame is always
// admitted into an empty window so a frame larger than a (misconfigured)
// window cannot deadlock; windows MUST be >= max_frame_bytes anyway.
func CanSend(f *Flow, taskUsed, connUsed, n int) bool {
	if f == nil {
		return true
	}
	taskOK := taskUsed+n <= f.TaskWindow || taskUsed == 0
	connOK := connUsed+n <= f.ConnWindow || connUsed == 0
	return taskOK && connOK
}

func (o *Outbox) wakeLocked() {
	for len(o.waiters) > 0 {
		w := o.waiters[0]
		if !o.closed && !o.fitsLocked(w.task, w.n) {
			return // strict FIFO: later waiters do not overtake
		}
		o.waiters = o.waiters[1:]
		close(w.ch)
	}
}

// Send assigns the next seq to env (a data frame of env.TaskID), waits for
// window space, records it as unacknowledged and writes it if connected.
// A failed write is not an error: the frame is retransmitted on resume.
func (o *Outbox) Send(ctx context.Context, env Envelope) (int64, error) {
	return o.SendFunc(ctx, env.TaskID, func(seq int64) Envelope { env.Seq = seq; return env })
}

// SendFunc is Send for frames whose body depends on their seq (E2EE sealing
// binds the seq into nonce and AAD). build MUST be deterministic in seq.
func (o *Outbox) SendFunc(ctx context.Context, taskID string, build func(seq int64) Envelope) (int64, error) {
	mk := func(seq int64) []byte {
		env := build(seq)
		env.V, env.TaskID, env.Seq = Version, taskID, seq
		raw, _ := json.Marshal(env)
		return raw
	}
	env := Envelope{TaskID: taskID}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return 0, ErrOutboxClosed
	}
	t := o.task(env.TaskID)
	// Size is computed with a provisional seq of the same magnitude.
	env.Seq = t.nextSeq + 1
	raw := mk(env.Seq)
	if len(o.waiters) > 0 || !o.fitsLocked(env.TaskID, len(raw)) {
		w := &waiter{task: env.TaskID, n: len(raw), ch: make(chan struct{})}
		o.waiters = append(o.waiters, w)
		o.mu.Unlock()
		select {
		case <-w.ch:
		case <-ctx.Done():
			o.mu.Lock()
			for i, x := range o.waiters {
				if x == w {
					o.waiters = append(o.waiters[:i], o.waiters[i+1:]...)
					break
				}
			}
			o.wakeLocked()
			o.mu.Unlock()
			return 0, ctx.Err()
		}
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			return 0, ErrOutboxClosed
		}
		t = o.task(env.TaskID)
		env.Seq = t.nextSeq + 1
		raw = mk(env.Seq)
	}
	t.nextSeq = env.Seq
	t.unacked = append(t.unacked, outFrame{env.Seq, raw})
	t.used += len(raw)
	o.connUsed += len(raw)
	o.mu.Unlock()
	_ = o.flush(env.TaskID)
	return env.Seq, nil
}

// Ack releases frames with seq <= seq.
func (o *Outbox) Ack(taskID string, seq int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.tasks[taskID]
	if t == nil {
		return
	}
	i := 0
	for ; i < len(t.unacked) && t.unacked[i].seq <= seq; i++ {
		t.used -= len(t.unacked[i].raw)
		o.connUsed -= len(t.unacked[i].raw)
	}
	t.unacked = t.unacked[i:]
	o.wakeLocked()
}

// Retransmit acknowledges frames <= peerRecv and rewrites the rest, in order.
func (o *Outbox) Retransmit(taskID string, peerRecv int64) error {
	o.Ack(taskID, peerRecv)
	o.wmu.Lock()
	o.mu.Lock()
	if t := o.tasks[taskID]; t != nil {
		t.written = peerRecv
	}
	o.mu.Unlock()
	o.wmu.Unlock()
	return o.flush(taskID)
}

// Unacked returns the number of unacknowledged frames and bytes of a task.
func (o *Outbox) Unacked(taskID string) (int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if t := o.tasks[taskID]; t != nil {
		return len(t.unacked), t.used
	}
	return 0, 0
}

// LastSeq is the last seq assigned for a task.
func (o *Outbox) LastSeq(taskID string) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	if t := o.tasks[taskID]; t != nil {
		return t.nextSeq
	}
	return 0
}

// Tasks lists tasks that still have state.
func (o *Outbox) Tasks() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for id := range o.tasks {
		out = append(out, id)
	}
	return out
}

// Forget drops a finished task's state.
func (o *Outbox) Forget(taskID string) {
	o.mu.Lock()
	if t := o.tasks[taskID]; t != nil {
		o.connUsed -= t.used
		delete(o.tasks, taskID)
	}
	o.wakeLocked()
	o.mu.Unlock()
}

// Close fails all blocked and future sends.
func (o *Outbox) Close() {
	o.mu.Lock()
	o.closed = true
	o.wakeLocked()
	o.mu.Unlock()
}

// Inbound is the receiving half: it detects duplicates and gaps per task and
// enforces the windows this side advertised.
type Inbound struct {
	mu       sync.Mutex
	flow     *Flow
	connUsed int
	tasks    map[string]*inTask
}

type inTask struct {
	recv   int64
	sizes  []inSize
	used   int
	closed bool
}

type inSize struct {
	seq int64
	n   int
}

func NewInbound(f *Flow) *Inbound { return &Inbound{flow: f, tasks: map[string]*inTask{}} }

// Verdict of Inbound.Accept.
const (
	Accepted  = iota // new, in-order frame
	Duplicate        // seq <= already received: drop silently (resume retransmit)
	Gap              // seq > recv+1: protocol error
	Overflow         // window exceeded: FLOW_CONTROL error
)

func (in *Inbound) Accept(taskID string, seq int64, size int) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	t := in.tasks[taskID]
	if t == nil {
		t = &inTask{}
		in.tasks[taskID] = t
	}
	switch {
	case seq <= t.recv:
		return Duplicate
	case seq != t.recv+1:
		return Gap
	}
	if in.flow != nil && ((t.used+size > in.flow.TaskWindow && t.used > 0) || (in.connUsed+size > in.flow.ConnWindow && in.connUsed > 0)) {
		return Overflow
	}
	t.recv = seq
	t.sizes = append(t.sizes, inSize{seq, size})
	t.used += size
	in.connUsed += size
	return Accepted
}

// Acked records that this side acknowledged up to seq (frees window).
func (in *Inbound) Acked(taskID string, seq int64) {
	in.mu.Lock()
	defer in.mu.Unlock()
	t := in.tasks[taskID]
	if t == nil {
		return
	}
	i := 0
	for ; i < len(t.sizes) && t.sizes[i].seq <= seq; i++ {
		t.used -= t.sizes[i].n
		in.connUsed -= t.sizes[i].n
	}
	t.sizes = t.sizes[i:]
}

// Recv is the highest contiguous seq received for a task.
func (in *Inbound) Recv(taskID string) int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	if t := in.tasks[taskID]; t != nil {
		return t.recv
	}
	return 0
}

// ResetWindows forgets in-flight accounting (after a reconnect the peer
// retransmits everything unacknowledged).
func (in *Inbound) ResetWindows() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.connUsed = 0
	for _, t := range in.tasks {
		t.used, t.sizes = 0, nil
	}
}

func (in *Inbound) Forget(taskID string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if t := in.tasks[taskID]; t != nil {
		in.connUsed -= t.used
		delete(in.tasks, taskID)
	}
}
