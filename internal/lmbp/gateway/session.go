package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// dstate is one device's protocol state. It outlives a single WebSocket
// connection for ResumeWindow so that a reconnecting device can resume.
type dstate struct {
	g                           *Gateway
	deviceID, tenantID, ownerID string
	out                         *lmbp.Outbox  // gateway→device data frames
	in                          *lmbp.Inbound // device→gateway data frames

	mu          sync.Mutex
	conn        *websocket.Conn
	wmu         *sync.Mutex
	gen         int
	models      []string
	modelCapMap map[string][]string
	caps        []string
	maxConc     int
	tokenExp    int64
	issuer      string // issuer the current session's token was verified against
	resumeToken string
	tasks       map[string]*gtask
	// reclaimed: tasks the gateway ended locally (cancel, timeout, slow
	// consumer...) whose device may still send frames. Those late frames
	// are ACKed so the device frees its resume buffer and flow credit;
	// the entry goes when the device's terminal frame arrives (bounded).
	reclaimed  map[string]int64 // task id → highest seq ACKed
	reclaimedQ []string
	slots      int
	slotWait   []chan struct{}
	dropped    bool
}

func (ds *dstate) connected() bool {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.conn != nil && !ds.dropped
}

func (ds *dstate) modelList() []string {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return append([]string(nil), ds.models...)
}

func (ds *dstate) modelCaps() map[string][]string {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	out := map[string][]string{}
	for k, v := range ds.modelCapMap {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (ds *dstate) capabilities() []string {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return append([]string{}, ds.caps...)
}

func (ds *dstate) has(capability string) bool {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return contains(ds.caps, capability)
}

func (ds *dstate) concurrency() int { ds.mu.Lock(); defer ds.mu.Unlock(); return ds.maxConc }

func (ds *dstate) modelHas(model, capability string) bool {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return contains(ds.modelCapMap[model], capability)
}

// acquire takes a concurrency slot, waiting in FIFO order (fair between
// callers) until ctx ends.
func (ds *dstate) acquire(ctx context.Context) bool {
	ds.mu.Lock()
	if ds.slots < ds.maxConc && len(ds.slotWait) == 0 {
		ds.slots++
		ds.mu.Unlock()
		return true
	}
	ch := make(chan struct{})
	ds.slotWait = append(ds.slotWait, ch)
	ds.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		ds.mu.Lock()
		defer ds.mu.Unlock()
		for i, c := range ds.slotWait {
			if c == ch {
				ds.slotWait = append(ds.slotWait[:i], ds.slotWait[i+1:]...)
				return false
			}
		}
		// Granted concurrently with cancellation: give it back.
		ds.releaseLocked()
		return false
	}
}

func (ds *dstate) release() { ds.mu.Lock(); ds.releaseLocked(); ds.mu.Unlock() }

func (ds *dstate) releaseLocked() {
	ds.slots--
	for len(ds.slotWait) > 0 && ds.slots < ds.maxConc {
		ch := ds.slotWait[0]
		ds.slotWait = ds.slotWait[1:]
		ds.slots++
		close(ch)
	}
}

// control writes a non-sequenced frame if the device is connected.
func (ds *dstate) control(e lmbp.Envelope) error {
	e.V = lmbp.Version
	b, _ := json.Marshal(e)
	ds.mu.Lock()
	conn, wmu := ds.conn, ds.wmu
	ds.mu.Unlock()
	if conn == nil {
		return websocket.ErrCloseSent
	}
	wmu.Lock()
	defer wmu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, b)
}

func (ds *dstate) sendErr(id, code, msg string) {
	b, _ := json.Marshal(lmbp.ErrorBody{Code: code, Message: msg})
	_ = ds.control(lmbp.Envelope{Type: lmbp.TypeError, ID: id, Data: b})
}

// dropDevice ends a device's state: live tasks fail and the socket closes.
func (g *Gateway) dropDevice(id, code, msg string) {
	g.mu.Lock()
	ds := g.devs[id]
	if ds != nil {
		delete(g.devs, id)
	}
	g.mu.Unlock()
	if ds != nil {
		ds.drop(code, msg)
	}
}

func (ds *dstate) drop(code, msg string) {
	ds.mu.Lock()
	ds.dropped = true
	conn := ds.conn
	tasks := make([]*gtask, 0, len(ds.tasks))
	for _, t := range ds.tasks {
		tasks = append(tasks, t)
	}
	waiters := ds.slotWait
	ds.slotWait = nil
	ds.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
	ds.out.Close()
	for _, t := range tasks {
		t.failOffline(code, msg)
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func (g *Gateway) connect(w http.ResponseWriter, r *http.Request) {
	if !contains(websocket.Subprotocols(r), lmbp.Subprotocol) {
		httpError(w, r, 400, lmbp.ErrVersion, "WebSocket subprotocol lmbp required")
		return
	}
	if r.Header.Get("Origin") != "" {
		httpError(w, r, 403, lmbp.ErrForbidden, "browser origins are not allowed")
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	iss := g.issuer(r)
	claims, err := g.verifyAccess(tok, iss)
	if err != nil {
		httpError(w, r, 401, lmbp.ErrUnauthenticated, err.Error())
		return
	}
	dev, err := g.cfg.Store.GetDevice(r.Context(), claims.Sub)
	if err != nil || dev.Status != "active" || dev.TenantID != claims.Tenant || claims.Cnf["jkt"] != lmbp.Thumbprint(dev.PublicKey) {
		httpError(w, r, 403, lmbp.ErrForbidden, "device revoked or token not bound to device key")
		return
	}
	up := websocket.Upgrader{Subprotocols: []string{lmbp.Subprotocol}, CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxFrame)
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var env lmbp.Envelope
	var hello lmbp.Hello
	mt, raw, err := conn.ReadMessage()
	if err != nil || mt != websocket.TextMessage || json.Unmarshal(raw, &env) != nil || env.Type != lmbp.TypeHello || json.Unmarshal(env.Data, &hello) != nil {
		return
	}
	wmu := &sync.Mutex{}
	early := func(code, msg string) {
		b, _ := json.Marshal(lmbp.ErrorBody{Code: code, Message: msg})
		_ = conn.WriteJSON(lmbp.Envelope{V: lmbp.Version, Type: lmbp.TypeError, ID: env.ID, Data: b})
	}
	if env.V != lmbp.Version {
		early(lmbp.ErrVersion, "unsupported protocol version")
		return
	}
	pc, perr := lmbp.VerifyProof(dev.PublicKey, hello.Proof, dev.ID, lmbp.ProofHello, tok, time.Now())
	if hello.DeviceID != dev.ID || perr != nil || !g.useJTI(pc.Jti, time.Now().Add(3*time.Minute)) {
		early(lmbp.ErrUnauthenticated, "invalid key proof")
		return
	}
	caps := intersect(hello.Capabilities, lmbp.Capabilities)
	if hello.E2EEKey != "" && contains(caps, lmbp.CapE2EE) {
		if _, _, err := lmbp.VerifyE2EEKey(dev.PublicKey, "", dev.ID, hello.E2EEKey, time.Now()); err != nil {
			early(lmbp.ErrBadRequest, "invalid e2ee key statement")
			return
		}
		_ = g.cfg.Store.SetDeviceE2EEKey(r.Context(), dev.ID, hello.E2EEKey)
	} else if contains(caps, lmbp.CapE2EE) {
		caps = removeS(caps, lmbp.CapE2EE) // no key, no E2EE
	}
	if hello.Flow != nil && contains(caps, lmbp.CapFlow) && !lmbp.ValidFlow(hello.Flow) {
		early(lmbp.ErrBadRequest, fmt.Sprintf("flow windows must be >= max_frame_bytes (%d) and conn_window >= task_window", lmbp.MaxFrameBytes))
		return
	}
	conc := hello.MaxConcurrency
	if conc < 1 {
		conc = 1
	}
	if conc > g.cfg.MaxConc {
		conc = g.cfg.MaxConc
	}

	// The device may have been revoked while we awaited the hello: the
	// upgrade-time read is stale. Re-check before activating.
	if cur, err := g.cfg.Store.GetDevice(r.Context(), dev.ID); err != nil || cur.Status != "active" {
		early(lmbp.ErrUnauthenticated, "device revoked")
		return
	}
	if activateHook != nil {
		activateHook(dev.ID)
	}

	// Resume or start fresh.
	g.mu.Lock()
	old := g.devs[dev.ID]
	resumed := old != nil && hello.Resume != nil && contains(caps, lmbp.CapResume) && hello.Resume.Token != "" && hello.Resume.Token == old.resumeTokenGet()
	if old != nil && !resumed {
		delete(g.devs, dev.ID)
	}
	ds := old
	if !resumed {
		ds = &dstate{g: g, deviceID: dev.ID, tenantID: dev.TenantID, ownerID: dev.OwnerID, out: lmbp.NewOutbox(), tasks: map[string]*gtask{}}
		g.devs[dev.ID] = ds
	}
	g.mu.Unlock()
	// A revoke that raced the activation (its drop ran before we registered).
	if cur, err := g.cfg.Store.GetDevice(r.Context(), dev.ID); err != nil || cur.Status != "active" {
		g.dropDevice(dev.ID, lmbp.ErrUnauthenticated, "device revoked")
	}
	if old != nil && !resumed {
		old.drop(lmbp.ErrOutcomeUnknown, "device reconnected without resuming")
	}
	gwFlow := g.cfg.Flow
	ds.mu.Lock()
	prevConn := ds.conn
	ds.gen++
	gen := ds.gen
	ds.conn, ds.wmu = conn, wmu
	ds.models, ds.modelCapMap, ds.caps, ds.maxConc, ds.tokenExp = hello.Models, hello.ModelCapabilities, caps, conc, claims.Exp
	ds.issuer = iss
	ds.resumeToken = lmbp.RandomID("rt_")
	if ds.in == nil {
		if contains(caps, lmbp.CapFlow) {
			ds.in = lmbp.NewInbound(&gwFlow)
		} else {
			ds.in = lmbp.NewInbound(nil)
		}
	} else {
		ds.in.ResetWindows() // the device retransmits everything we did not ack
	}
	// Per-task recv positions survive a resume.
	var resumeTasks []lmbp.ResumeTask
	deviceRecv := map[string]int64{}
	var lost []*gtask
	if resumed {
		for _, rt := range hello.Resume.Tasks {
			deviceRecv[rt.TaskID] = rt.RecvSeq
		}
		for id, t := range ds.tasks {
			if _, ok := deviceRecv[id]; !ok && t.sentRun.Load() {
				lost = append(lost, t) // device no longer has it: never re-executed
				continue
			}
			resumeTasks = append(resumeTasks, lmbp.ResumeTask{TaskID: id, RecvSeq: ds.in.Recv(id)})
		}
	}
	tokenOut := ds.resumeToken
	ds.mu.Unlock()
	if prevConn != nil {
		_ = prevConn.Close()
	}
	for _, t := range lost {
		t.failOffline(lmbp.ErrOutcomeUnknown, "device resumed without this task; outcome unknown")
	}
	if contains(caps, lmbp.CapFlow) {
		f := hello.Flow
		if f == nil {
			f = &lmbp.Flow{TaskWindow: lmbp.DefaultTaskWindow, ConnWindow: lmbp.DefaultConnWindow}
		}
		ds.out.SetFlow(f)
	} else {
		ds.out.SetFlow(nil)
	}
	welcome := lmbp.Welcome{SessionID: lmbp.RandomID("sess_"), HeartbeatSeconds: int(g.cfg.PingInterval / time.Second), MaxFrameBytes: maxFrame,
		TokenExpiresAt: claims.Exp, Capabilities: caps, MaxConcurrency: conc}
	if welcome.HeartbeatSeconds < 1 {
		welcome.HeartbeatSeconds = 1
	}
	if contains(caps, lmbp.CapFlow) {
		welcome.Flow = &gwFlow
	}
	if contains(caps, lmbp.CapResume) {
		welcome.ResumeToken, welcome.ResumeSeconds = tokenOut, int(g.cfg.ResumeWindow/time.Second)
		if hello.Resume != nil {
			welcome.Resume = &lmbp.WelcomeResume{Resumed: resumed, Tasks: resumeTasks}
			if welcome.Resume.Tasks == nil {
				welcome.Resume.Tasks = []lmbp.ResumeTask{}
			}
		}
	}
	wb, _ := json.Marshal(welcome)
	if ds.control(lmbp.Envelope{Type: lmbp.TypeWelcome, ID: env.ID, Data: wb}) != nil {
		return
	}
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.TextMessage, b)
	}
	wgen := ds.out.AttachOwned(write)
	if resumed {
		g.resumes.Add(1)
		for _, rt := range resumeTasks {
			_ = ds.out.Retransmit(rt.TaskID, deviceRecv[rt.TaskID])
		}
		g.cfg.Logger.Printf("device %s resumed %d task(s)", dev.ID, len(resumeTasks))
	} else {
		g.cfg.Logger.Printf("device %s (tenant %s) online, models=%v caps=%v conc=%d", dev.ID, dev.TenantID, hello.Models, caps, conc)
	}
	closed := make(chan struct{})
	defer func() {
		close(closed)
		ds.out.Detach(wgen) // never clears a newer connection's sender
		ds.mu.Lock()
		current := ds.gen == gen
		if current {
			ds.conn = nil
		}
		canResume := contains(ds.caps, lmbp.CapResume) && !ds.dropped
		ds.mu.Unlock()
		if !current {
			return // replaced by a newer connection
		}
		if !canResume {
			g.dropDevice(dev.ID, lmbp.ErrDeviceOffline, "device disconnected")
			return
		}
		time.AfterFunc(g.cfg.ResumeWindow, func() {
			ds.mu.Lock()
			expired := ds.gen == gen && ds.conn == nil
			ds.mu.Unlock()
			if expired {
				g.mu.Lock()
				if g.devs[dev.ID] == ds {
					delete(g.devs, dev.ID)
				}
				g.mu.Unlock()
				ds.drop(lmbp.ErrDeviceOffline, "device did not resume in time")
			}
		})
	}()
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	go func() {
		t := time.NewTicker(g.cfg.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-closed:
				return
			case <-t.C:
				ds.mu.Lock()
				expired := time.Now().Unix() > ds.tokenExp+30
				ds.mu.Unlock()
				if expired {
					_ = conn.Close()
					return
				}
				wmu.Lock()
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				wmu.Unlock()
			}
		}
	}()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		mt, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseUnsupportedData, "text frames only"), time.Now().Add(time.Second))
			return
		}
		var in lmbp.Envelope
		if json.Unmarshal(raw, &in) != nil || in.V != lmbp.Version {
			ds.sendErr(in.ID, lmbp.ErrBadRequest, "malformed frame")
			continue
		}
		switch in.Type {
		case lmbp.TypeTaskDelta, lmbp.TypeTaskDone, lmbp.TypeTaskError:
			if !g.onDeviceData(ds, in, len(raw)) {
				return
			}
		case lmbp.TypeTaskAck:
			var a lmbp.TaskAck
			if json.Unmarshal(in.Data, &a) == nil {
				ds.out.Ack(in.TaskID, a.Seq)
			}
		case lmbp.TypeSessionToken:
			if !g.refreshSession(r.Context(), ds, dev, in) {
				ds.sendErr(in.ID, lmbp.ErrUnauthenticated, "session token rejected")
				return
			}
		case lmbp.TypeModelsUpdate:
			var mu lmbp.ModelsUpdate
			if json.Unmarshal(in.Data, &mu) == nil && len(mu.Models) <= 500 {
				ds.mu.Lock()
				ds.models, ds.modelCapMap = mu.Models, mu.ModelCapabilities
				if mu.MaxConcurrency >= 1 {
					ds.maxConc = min(mu.MaxConcurrency, g.cfg.MaxConc)
				}
				hasE2EE := contains(ds.caps, lmbp.CapE2EE)
				ds.mu.Unlock()
				if mu.E2EEKey != "" && hasE2EE {
					if _, _, err := lmbp.VerifyE2EEKey(dev.PublicKey, "", dev.ID, mu.E2EEKey, time.Now()); err == nil {
						_ = g.cfg.Store.SetDeviceE2EEKey(context.Background(), dev.ID, mu.E2EEKey)
					}
				}
			}
		case lmbp.TypePing:
			_ = ds.control(lmbp.Envelope{Type: lmbp.TypePong, ID: in.ID})
		case lmbp.TypePong:
		default:
			ds.sendErr(in.ID, lmbp.ErrBadRequest, "unknown message type "+in.Type)
		}
	}
}

// activateHook runs after the hello re-check, before the session is
// registered (tests).
var activateHook func(deviceID string)

// maxReclaimed bounds the late-frame state kept per device.
const maxReclaimed = 1024

// reclaimLocked remembers a locally ended task (ds.mu held).
func (ds *dstate) reclaimLocked(id string, acked int64) {
	if ds.reclaimed == nil {
		ds.reclaimed = map[string]int64{}
	}
	if _, ok := ds.reclaimed[id]; !ok {
		ds.reclaimedQ = append(ds.reclaimedQ, id)
	}
	ds.reclaimed[id] = acked
	if len(ds.reclaimedQ) > 2*maxReclaimed { // drop ids already finished
		q := ds.reclaimedQ[:0]
		for _, x := range ds.reclaimedQ {
			if _, ok := ds.reclaimed[x]; ok {
				q = append(q, x)
			}
		}
		ds.reclaimedQ = q
	}
	for len(ds.reclaimed) > maxReclaimed && len(ds.reclaimedQ) > 0 {
		delete(ds.reclaimed, ds.reclaimedQ[0]) // oldest first
		ds.reclaimedQ = ds.reclaimedQ[1:]
	}
}

func (ds *dstate) resumeTokenGet() string { ds.mu.Lock(); defer ds.mu.Unlock(); return ds.resumeToken }

// onDeviceData handles task.delta/done/error. It returns false when the
// connection must be closed (flow-control violation).
// deviceDataHook, when set by a test, sees each device data frame as the
// read loop hands it over, before any gateway state is touched. Nil in
// production.
// The returned func, if any, runs once the frame is fully handled.
var deviceDataHook atomic.Pointer[func(in lmbp.Envelope) func()]

func (g *Gateway) onDeviceData(ds *dstate, in lmbp.Envelope, size int) bool {
	if h := deviceDataHook.Load(); h != nil {
		if after := (*h)(in); after != nil {
			defer after()
		}
	}
	// Lookup and Accept are one step under ds.mu, serialized with the local
	// finish transition in finishWith (Review8 P2-a).
	ds.mu.Lock()
	t := ds.tasks[in.TaskID]
	inb := ds.in
	var res int
	if t != nil {
		res = inb.Accept(in.TaskID, in.Seq, size)
		if res == lmbp.Accepted && isTerminal(in.Type) {
			t.devTerminal.Store(true) // seen by finishWith under the same lock
		}
	}
	ds.mu.Unlock()
	if t == nil {
		ds.mu.Lock()
		acked, late := ds.reclaimed[in.TaskID]
		if late {
			if in.Seq > acked {
				ds.reclaimed[in.TaskID] = in.Seq
			}
			if in.Type == lmbp.TypeTaskDone || in.Type == lmbp.TypeTaskError {
				delete(ds.reclaimed, in.TaskID)
			}
		}
		ds.mu.Unlock()
		if late { // ended here, still running there: release its credit
			if in.Seq > acked {
				b, _ := json.Marshal(lmbp.TaskAck{Seq: in.Seq})
				_ = ds.control(lmbp.Envelope{Type: lmbp.TypeTaskAck, TaskID: in.TaskID, Data: b})
			}
			return true
		}
		b, _ := json.Marshal(lmbp.TaskCancel{Reason: "unknown_task"})
		_ = ds.control(lmbp.Envelope{Type: lmbp.TypeTaskCancel, TaskID: in.TaskID, Data: b})
		return true
	}
	switch res {
	case lmbp.Duplicate:
		t.maybeAck() // the device may have missed our ack
		return true
	case lmbp.Gap:
		t.cancel("protocol_error")
		t.failLocal(lmbp.ErrInternal, "sequence gap from device", false)
		return true
	case lmbp.Overflow:
		ds.sendErr("", lmbp.ErrFlowControl, "flow-control window exceeded")
		t.failLocal(lmbp.ErrFlowControl, "device exceeded the flow-control window", false)
		return false
	}
	t.onDeviceFrame(in)
	return true
}

// refreshSession validates a mid-connection token and extends its expiry.
func (g *Gateway) refreshSession(ctx context.Context, ds *dstate, dev store.Device, in lmbp.Envelope) bool {
	var st lmbp.SessionToken
	if json.Unmarshal(in.Data, &st) != nil {
		return false
	}
	ds.mu.Lock()
	iss := ds.issuer
	ds.mu.Unlock()
	c, err := g.verifyAccess(st.AccessToken, iss)
	if err != nil || c.Sub != dev.ID || c.Cnf["jkt"] != lmbp.Thumbprint(dev.PublicKey) {
		return false
	}
	cur, err := g.cfg.Store.GetDevice(ctx, dev.ID)
	if err != nil || cur.Status != "active" {
		return false
	}
	pc, err := lmbp.VerifyProof(dev.PublicKey, st.Proof, dev.ID, lmbp.ProofSessionToken, st.AccessToken, time.Now())
	if err != nil || !g.useJTI(pc.Jti, time.Now().Add(3*time.Minute)) {
		return false
	}
	ds.mu.Lock()
	ds.tokenExp = c.Exp
	ds.mu.Unlock()
	g.refreshes.Add(1)
	b, _ := json.Marshal(lmbp.SessionToken{ExpiresAt: c.Exp})
	return ds.control(lmbp.Envelope{Type: lmbp.TypeSessionToken, ID: in.ID, Data: b}) == nil
}

func removeS(xs []string, x string) []string {
	out := xs[:0:0]
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}
