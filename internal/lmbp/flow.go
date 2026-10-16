package lmbp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// Reliable, flow-controlled delivery of data frames (docs/lmbp-spec.md §8).
//
// Each side numbers its data frames per task (seq 1, 2, ...). A frame stays
// "unacknowledged" until the peer sends task.ack{seq>=n}. The peer advertises
// two receive windows (Flow): the sender MUST keep the total size (bytes of
// the serialized frame) of unacknowledged frames <= task_window per task and
// <= conn_window across all tasks. Unacknowledged frames are exactly what is
// retransmitted after a resume, so the windows also bound resume buffers.

var ErrOutboxClosed = errors.New("outbox closed")

// ErrTaskForgotten: the task was dropped with Forget; its sends fail and its
// state is never re-created.
var ErrTaskForgotten = errors.New("task forgotten")

// maxForgotten bounds the remembered forgotten task ids (oldest dropped first).
const maxForgotten = 4096

// ErrFrameTooLarge: the final encoded frame exceeds MaxFrameBytes. Such a
// frame is never enqueued (no seq, no credit); the sender must split or fail.
var ErrFrameTooLarge = errors.New("encoded frame exceeds max_frame_bytes")

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
	mk   func(seq int64) []byte
	ch   chan struct{} // closed once committed (seq set) or failed (err set)
	seq  int64
	err  error
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
	writeGen uint64     // generation of write (AttachOwned / Detach)
	wmu      sync.Mutex // serializes flush so frames go out in seq order
	closed   bool
	// forgotten: tasks dropped with Forget (bounded FIFO). A send still
	// waiting for credit, or arriving later, fails instead of re-creating
	// state that no retransmit list covers. Review9 P2-7.
	forgotten     map[string]struct{}
	forgottenFIFO []string
}

func NewOutbox() *Outbox { return &Outbox{tasks: map[string]*outTask{}, forgotten: map[string]struct{}{}} }

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
	o.AttachOwned(write)
}

// AttachOwned installs write and returns its generation; only Detach with
// that generation removes it, so an old connection closing late can never
// clear a newer connection's sender (install and clear are atomic under
// o.mu).
func (o *Outbox) AttachOwned(write func([]byte) error) uint64 {
	o.mu.Lock()
	o.writeGen++
	gen := o.writeGen
	o.write = write
	for _, t := range o.tasks {
		t.written = -1
	}
	o.mu.Unlock()
	return gen
}

// Detach removes the sender installed as gen, if it is still the current one.
func (o *Outbox) Detach(gen uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.writeGen != gen {
		return // a newer connection owns the outbox now
	}
	o.write = nil
	for _, t := range o.tasks {
		t.written = -1
	}
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

// CanSend is the normative sender rule (docs/lmbp-spec.md §15.3): a data
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

// wakeLocked admits queued sends in strict FIFO order. Admission and credit
// reservation are one atomic step under o.mu: the seq is assigned, the frame
// built, recorded unacked and charged to both windows before the waiter is
// released, so concurrent senders can never jointly overshoot a window
// (audit finding 2).
func (o *Outbox) wakeLocked() {
	for len(o.waiters) > 0 {
		w := o.waiters[0]
		if o.closed {
			o.waiters = o.waiters[1:]
			w.err = ErrOutboxClosed
			close(w.ch)
			continue
		}
		if _, gone := o.forgotten[w.task]; gone {
			o.waiters = o.waiters[1:]
			w.err = ErrTaskForgotten
			close(w.ch)
			continue
		}
		t := o.task(w.task)
		seq := t.nextSeq + 1
		raw := w.mk(seq)
		if len(raw) > MaxFrameBytes { // checked on the final wire bytes
			o.waiters = o.waiters[1:]
			w.err = ErrFrameTooLarge
			close(w.ch)
			continue
		}
		if !o.fitsLocked(w.task, len(raw)) {
			return // strict FIFO: later waiters do not overtake
		}
		o.waiters = o.waiters[1:]
		o.commitLocked(t, seq, raw)
		w.seq = seq
		close(w.ch)
	}
}

func (o *Outbox) commitLocked(t *outTask, seq int64, raw []byte) {
	t.nextSeq = seq
	t.unacked = append(t.unacked, outFrame{seq, raw})
	t.used += len(raw)
	o.connUsed += len(raw)
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
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return 0, ErrOutboxClosed
	}
	if _, gone := o.forgotten[taskID]; gone {
		o.mu.Unlock()
		return 0, ErrTaskForgotten
	}
	w := &waiter{task: taskID, mk: mk, ch: make(chan struct{})}
	o.waiters = append(o.waiters, w)
	o.wakeLocked() // commits w immediately when it is first and fits
	o.mu.Unlock()
	select {
	case <-w.ch:
	case <-ctx.Done():
		o.mu.Lock()
		queued := false
		for i, x := range o.waiters {
			if x == w {
				o.waiters = append(o.waiters[:i], o.waiters[i+1:]...)
				queued = true
				break
			}
		}
		o.wakeLocked()
		o.mu.Unlock()
		if queued {
			return 0, ctx.Err()
		}
		<-w.ch // admitted concurrently with cancellation: the frame is committed
	}
	if w.err != nil {
		return 0, w.err
	}
	_ = o.flush(taskID)
	return w.seq, nil
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

// Forget drops a task's state for good: its credit is released, a send
// still waiting for credit fails, and later sends for it fail too, so no
// unreachable state (paused, outside any retransmit list) is re-created.
func (o *Outbox) Forget(taskID string) {
	o.mu.Lock()
	if t := o.tasks[taskID]; t != nil {
		o.connUsed -= t.used
		delete(o.tasks, taskID)
	}
	if _, ok := o.forgotten[taskID]; !ok {
		if o.forgotten == nil {
			o.forgotten = map[string]struct{}{}
		}
		o.forgotten[taskID] = struct{}{}
		o.forgottenFIFO = append(o.forgottenFIFO, taskID)
		if len(o.forgottenFIFO) > maxForgotten {
			delete(o.forgotten, o.forgottenFIFO[0])
			o.forgottenFIFO = o.forgottenFIFO[1:]
		}
	}
	// Fail this task's queued sends wherever they are in the queue.
	kept := o.waiters[:0]
	for _, w := range o.waiters {
		if w.task == taskID {
			w.err = ErrTaskForgotten
			close(w.ch)
			continue
		}
		kept = append(kept, w)
	}
	o.waiters = kept
	o.wakeLocked()
	o.mu.Unlock()
}

// Close fails all blocked and future sends.
// Usage reports unacknowledged frames and bytes over all tasks (the
// connection window in use).
func (o *Outbox) Usage() (frames, bytes int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, t := range o.tasks {
		frames += len(t.unacked)
	}
	return frames, o.connUsed
}

// Waiting reports how many sends are queued for flow credit.
func (o *Outbox) Waiting() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.waiters)
}

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
