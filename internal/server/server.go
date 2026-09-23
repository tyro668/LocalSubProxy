package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"localsubproxy/internal/durable"
	"localsubproxy/internal/protocol"
)

type Config struct {
	DataFile, RemoteToken, DeviceID, DeviceToken string
	RequireBinding                               bool
}

type record struct {
	Task        protocol.Task `json:"task"`
	Grant       string        `json:"grant,omitempty"`
	GrantUntil  time.Time     `json:"grant_until,omitempty"`
	RunDeadline time.Time     `json:"run_deadline,omitempty"`
}

type snapshot struct {
	Tasks           map[string]*record         `json:"tasks"`
	Idem            map[string]string          `json:"idempotency"`
	Artifacts       map[string]*artifactRecord `json:"artifacts,omitempty"`
	SessionEpoch    int64                      `json:"session_epoch"`
	Revoked         bool                       `json:"revoked,omitempty"`
	OwnerID         string                     `json:"owner_id,omitempty"`
	PairingHash     string                     `json:"pairing_hash,omitempty"`
	PairingExpires  time.Time                  `json:"pairing_expires,omitempty"`
	PairingAttempts int                        `json:"pairing_attempts,omitempty"`
	PairingOwnerID  string                     `json:"pairing_owner_id,omitempty"`
}

type Server struct {
	mu                  sync.Mutex
	cfg                 Config
	state               snapshot
	adapter             protocol.AdapterHealth
	adapters            map[string]protocol.AdapterHealth
	lastSeen            time.Time
	lastSnapshotVersion int64
	changed             chan struct{}
	wsMu                sync.Mutex
	active              *wsSession
}

func New(cfg Config) (*Server, error) {
	if cfg.DataFile == "" || len(cfg.RemoteToken) < 32 || len(cfg.DeviceToken) < 32 || cfg.DeviceID == "" {
		return nil, errors.New("data file, device ID, and independent 32+ character remote service/device tokens are required")
	}
	s := &Server{cfg: cfg, state: snapshot{Tasks: map[string]*record{}, Idem: map[string]string{}, Artifacts: map[string]*artifactRecord{}}, adapters: map[string]protocol.AdapterHealth{}, changed: make(chan struct{})}
	if err := durable.ReadJSON(cfg.DataFile, &s.state); err != nil {
		return nil, err
	}
	if s.state.Tasks == nil {
		s.state.Tasks = map[string]*record{}
	}
	if s.state.Idem == nil {
		s.state.Idem = map[string]string{}
	}
	if s.state.Artifacts == nil {
		s.state.Artifacts = map[string]*artifactRecord{}
	}
	if err := s.pruneArtifacts(); err != nil {
		return nil, err
	}
	if err := s.persist(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) persist() error { return durable.WriteJSON(s.cfg.DataFile, s.state) }
func (s *Server) notify()        { close(s.changed); s.changed = make(chan struct{}) }

func randomID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

func secureEqual(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }
func (s *Server) auth(w http.ResponseWriter, r *http.Request, device bool) bool {
	want := s.cfg.RemoteToken
	if device {
		want = s.cfg.DeviceToken
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") || !secureEqual(strings.TrimPrefix(authorization, "Bearer "), want) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if device {
		s.mu.Lock()
		revoked := s.state.Revoked
		unbound := s.cfg.RequireBinding && s.state.OwnerID == ""
		s.mu.Unlock()
		if revoked {
			http.Error(w, "device revoked", http.StatusForbidden)
			return false
		}
		if unbound {
			http.Error(w, "device not bound", http.StatusForbidden)
			return false
		}
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, v any, max int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), 400)
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		http.Error(w, "unexpected trailing JSON", 400)
		return false
	}
	return true
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func method(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method != want {
		w.Header().Set("Allow", want)
		http.Error(w, "method not allowed", 405)
		return false
	}
	return true
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !method(w, r, "GET") {
			return
		}
		reply(w, 200, map[string]string{"status": "ok"})
	})
	m.HandleFunc("/v1/connectors", s.connectors)
	m.HandleFunc("/v1/connectors/", s.connectorRoute)
	m.HandleFunc("/v1/task-artifacts", s.artifacts)
	m.HandleFunc("/v1/llm-tasks", s.tasks)
	m.HandleFunc("/v1/llm-tasks/", s.taskRoute)
	m.HandleFunc("/v1/models", s.openAIModels)
	m.HandleFunc("/v1/chat/completions", s.openAIChat)
	m.HandleFunc("/v1/connector/heartbeat", s.httpDevice(s.heartbeat))
	m.HandleFunc("/v1/connector/commands", s.commands)
	m.HandleFunc("/v1/connector/accepted", s.httpDevice(s.accepted))
	m.HandleFunc("/v1/connector/start", s.httpDevice(s.start))
	m.HandleFunc("/v1/connector/lease", s.httpDevice(s.lease))
	m.HandleFunc("/v1/connector/events", s.httpDevice(s.events))
	m.HandleFunc("/v1/connector/connect", s.connectWS)
	m.HandleFunc("/v1/connector/pairing", s.pairing)
	m.HandleFunc("/v1/connector/claim", s.claimPairing)
	m.HandleFunc("/v1/connector/artifacts/", s.deviceArtifact)
	return m
}

func (s *Server) connectors(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, false) || !method(w, r, "GET") {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	status := "offline"
	if s.state.Revoked {
		status = "revoked"
	} else if time.Since(s.lastSeen) < 75*time.Second {
		status = "online"
	}
	list := make([]protocol.AdapterHealth, 0, len(s.adapters))
	for _, a := range s.adapters {
		list = append(list, a)
	}
	reply(w, 200, map[string]any{"devices": []any{map[string]any{"device_id": s.cfg.DeviceID, "status": status, "last_seen": s.lastSeen, "adapter": s.adapter, "adapters": list, "owner_id": s.state.OwnerID, "binding_required": s.cfg.RequireBinding}}})
}

func (s *Server) connectorRoute(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, false) {
		return
	}
	if r.URL.Path == "/v1/connectors/"+s.cfg.DeviceID+"/bind" {
		s.bind(w, r)
		return
	}
	if r.URL.Path == "/v1/connectors/"+s.cfg.DeviceID+"/pairing-code" {
		s.issuePairing(w, r)
		return
	}
	if r.URL.Path != "/v1/connectors/"+s.cfg.DeviceID+"/revoke" {
		http.NotFound(w, r)
		return
	}
	if !method(w, r, http.MethodPost) {
		return
	}
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	s.mu.Lock()
	if !s.state.Revoked {
		s.state.Revoked = true
		previous := map[string]protocol.Task{}
		for id, rec := range s.state.Tasks {
			if terminal(rec.Task.State) {
				continue
			}
			previous[id] = rec.Task
			if rec.Task.State == "QUEUED" || rec.Task.State == "DISPATCHED" {
				rec.Task.State = "CANCELLED"
			} else {
				rec.Task.State = "CANCEL_REQUESTED"
			}
			rec.Task.UpdatedAt = time.Now().UTC()
		}
		if err := s.persist(); err != nil {
			s.state.Revoked = false
			for id, task := range previous {
				s.state.Tasks[id].Task = task
			}
			s.mu.Unlock()
			http.Error(w, "storage error", 503)
			return
		}
		s.notify()
	}
	s.mu.Unlock()
	if s.active != nil {
		_ = s.active.conn.Close()
	}
	reply(w, 200, map[string]any{"device_id": s.cfg.DeviceID, "status": "revoked"})
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, false) || !method(w, r, "POST") {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		http.Error(w, "Idempotency-Key must be 8-128 characters", 400)
		return
	}
	var req protocol.CreateTask
	if !decode(w, r, &req, 2<<20) {
		return
	}
	if err := req.Validate(); err != nil {
		http.Error(w, err.Error(), 422)
		return
	}
	if req.Target.DeviceID != s.cfg.DeviceID {
		http.Error(w, "device not found", 404)
		return
	}
	if req.Options.QueueTTLSeconds == 0 {
		req.Options.QueueTTLSeconds = 300
	}
	if req.Options.MaxRuntimeSeconds == 0 {
		req.Options.MaxRuntimeSeconds = 600
	}
	if req.Options.QueueTTLSeconds < 1 || req.Options.QueueTTLSeconds > 3600 || req.Options.MaxRuntimeSeconds < 1 || req.Options.MaxRuntimeSeconds > 3600 {
		http.Error(w, "invalid timeout", 422)
		return
	}
	digest := protocol.Digest(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Revoked {
		http.Error(w, "device revoked", 403)
		return
	}
	if s.cfg.RequireBinding && s.state.OwnerID == "" {
		http.Error(w, "device not bound", 403)
		return
	}
	if id, ok := s.state.Idem[key]; ok {
		if rec := s.state.Tasks[id]; rec != nil {
			if rec.Task.Digest != digest {
				http.Error(w, "idempotency conflict", 409)
				return
			}
			reply(w, 202, rec.Task)
			return
		}
	}
	for _, id := range req.Input.ArtifactRefs {
		meta := s.state.Artifacts[id]
		if meta == nil || meta.TaskID != "" {
			http.Error(w, "artifact not available", 422)
			return
		}
	}
	a, known := s.adapters[req.Target.AdapterID]
	if len(s.adapters) > 0 && (!known || !contains(a.Models, req.Target.ModelID)) {
		http.Error(w, "model not available on device", 422)
		return
	}
	if known && len(a.Operations) > 0 && !contains(a.Operations, req.Operation) {
		http.Error(w, "operation not supported by adapter", 422)
		return
	}
	now := time.Now().UTC()
	t := protocol.Task{ID: randomID("task_"), State: "QUEUED", Request: req, Digest: digest, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Duration(req.Options.QueueTTLSeconds) * time.Second)}
	s.state.Tasks[t.ID] = &record{Task: t}
	s.state.Idem[key] = t.ID
	for _, id := range req.Input.ArtifactRefs {
		s.state.Artifacts[id].TaskID = t.ID
	}
	if err := s.persist(); err != nil {
		delete(s.state.Tasks, t.ID)
		delete(s.state.Idem, key)
		for _, id := range req.Input.ArtifactRefs {
			s.state.Artifacts[id].TaskID = ""
		}
		http.Error(w, "storage error", 503)
		return
	}
	s.notify()
	reply(w, 202, t)
}
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (s *Server) taskRoute(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, false) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/llm-tasks/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	switch {
	case len(parts) == 1 && r.Method == "GET":
		s.getTask(w, parts[0])
	case len(parts) == 2 && parts[1] == "cancel" && r.Method == "POST":
		s.cancel(w, parts[0])
	case len(parts) == 2 && parts[1] == "events" && r.Method == "GET":
		s.stream(w, r, parts[0])
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) getTask(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.expireLocked(); err != nil {
		http.Error(w, "storage error", 503)
		return
	}
	if err := s.expireLeasesLocked(); err != nil {
		http.Error(w, "storage error", 503)
		return
	}
	rec := s.state.Tasks[id]
	if rec == nil {
		http.Error(w, "not found", 404)
		return
	}
	reply(w, 200, rec.Task)
}

// expireLocked closes tasks that have not been accepted before their queue deadline.
func (s *Server) expireLocked() error {
	now := time.Now().UTC()
	previous := map[string]protocol.Task{}
	for id, rec := range s.state.Tasks {
		if (rec.Task.State == "QUEUED" || rec.Task.State == "DISPATCHED") && now.After(rec.Task.ExpiresAt) {
			previous[id] = rec.Task
			rec.Task.State = "EXPIRED"
			rec.Task.UpdatedAt = now
		}
	}
	if len(previous) == 0 {
		return nil
	}
	if err := s.persist(); err != nil {
		for id, task := range previous {
			s.state.Tasks[id].Task = task
		}
		return err
	}
	s.notify()
	return nil
}

func (s *Server) expireLeasesLocked() error {
	now := time.Now().UTC()
	previous := map[string]protocol.Task{}
	for id, rec := range s.state.Tasks {
		if rec.Task.State == "RUNNING" && !rec.Task.LeaseUntil.IsZero() && now.After(rec.Task.LeaseUntil) {
			previous[id] = rec.Task
			rec.Task.State = "RECONCILING"
			rec.Task.UpdatedAt = now
		}
	}
	if len(previous) == 0 {
		return nil
	}
	if err := s.persist(); err != nil {
		for id, task := range previous {
			s.state.Tasks[id].Task = task
		}
		return err
	}
	s.notify()
	return nil
}
func terminal(state string) bool {
	switch state {
	case "SUCCEEDED", "FAILED", "CANCELLED", "EXPIRED", "UNKNOWN":
		return true
	}
	return false
}
func (s *Server) cancel(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.state.Tasks[id]
	if rec == nil {
		http.Error(w, "not found", 404)
		return
	}
	if terminal(rec.Task.State) {
		reply(w, 200, rec.Task)
		return
	}
	before := rec.Task
	if rec.Task.State == "QUEUED" || rec.Task.State == "DISPATCHED" {
		rec.Task.State = "CANCELLED"
	} else {
		rec.Task.State = "CANCEL_REQUESTED"
	}
	rec.Task.UpdatedAt = time.Now().UTC()
	if err := s.persist(); err != nil {
		rec.Task = before
		http.Error(w, "storage error", 503)
		return
	}
	s.notify()
	reply(w, 202, rec.Task)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, id string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unavailable", 500)
		return
	}
	last := r.Header.Get("Last-Event-ID")
	seq := int64(0)
	if last != "" {
		p := strings.LastIndex(last, ":")
		if p < 0 {
			http.Error(w, "invalid Last-Event-ID", 400)
			return
		}
		if _, err := fmt.Sscan(last[p+1:], &seq); err != nil {
			http.Error(w, "invalid Last-Event-ID", 400)
			return
		}
	}
	s.mu.Lock()
	rec := s.state.Tasks[id]
	s.mu.Unlock()
	if rec == nil {
		http.Error(w, "not found", 404)
		return
	}
	if last != "" && strings.TrimSuffix(last, fmt.Sprintf(":%d", seq)) != rec.Task.AttemptID {
		http.Error(w, "Last-Event-ID attempt mismatch", 400)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	for {
		s.mu.Lock()
		if err := s.expireLocked(); err != nil {
			s.mu.Unlock()
			return
		}
		if err := s.expireLeasesLocked(); err != nil {
			s.mu.Unlock()
			return
		}
		rec = s.state.Tasks[id]
		events := append([]protocol.Event(nil), rec.Task.Events...)
		state := rec.Task.State
		attempt := rec.Task.AttemptID
		ch := s.changed
		s.mu.Unlock()
		for _, ev := range events {
			if ev.Seq <= seq {
				continue
			}
			b, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "id: %s:%d\nevent: %s\ndata: %s\n\n", attempt, ev.Seq, ev.Kind, b); err != nil {
				return
			}
			seq = ev.Seq
			flusher.Flush()
		}
		if terminal(state) {
			fmt.Fprintf(w, "event: state\ndata: %q\n\n", state)
			flusher.Flush()
			return
		}
		select {
		case <-ch:
		case <-r.Context().Done():
			return
		case <-time.After(15 * time.Second):
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, true) || !method(w, r, "POST") {
		return
	}
	var hb protocol.Heartbeat
	if !decode(w, r, &hb, 64<<10) {
		return
	}
	if hb.Protocol != protocol.Version || hb.DeviceID != s.cfg.DeviceID {
		http.Error(w, "protocol or device mismatch", 409)
		return
	}
	s.mu.Lock()
	s.setAdapters(hb)
	s.lastSeen = time.Now()
	s.notify()
	s.mu.Unlock()
	reply(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) setAdapters(hb protocol.Heartbeat) {
	if hb.SnapshotVersion > 0 && hb.SnapshotVersion < s.lastSnapshotVersion {
		return
	}
	if hb.SnapshotVersion > 0 {
		s.lastSnapshotVersion = hb.SnapshotVersion
	}
	s.adapters = map[string]protocol.AdapterHealth{}
	list := hb.Adapters
	if len(list) == 0 && hb.Adapter.AdapterID != "" {
		list = []protocol.AdapterHealth{hb.Adapter}
	}
	for _, a := range list {
		if a.AdapterID != "" {
			s.adapters[a.AdapterID] = a
		}
	}
	s.adapter = hb.Adapter
	if s.adapter.AdapterID == "" && len(list) > 0 {
		s.adapter = list[0]
	}
}

func (s *Server) sign(c protocol.Command) string {
	c.Signature = ""
	b, _ := json.Marshal(c)
	m := hmac.New(sha256.New, []byte(s.cfg.DeviceToken))
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}
func VerifyCommand(c protocol.Command, token string) bool {
	sig := c.Signature
	c.Signature = ""
	b, _ := json.Marshal(c)
	m := hmac.New(sha256.New, []byte(token))
	m.Write(b)
	expected := hex.EncodeToString(m.Sum(nil))
	return secureEqual(sig, expected)
}
func (s *Server) nextCommand() *protocol.Command {
	if s.state.Revoked || (s.cfg.RequireBinding && s.state.OwnerID == "") {
		return nil
	}
	now := time.Now().UTC()
	for _, rec := range s.state.Tasks {
		t := &rec.Task
		if t.Request.Target.DeviceID != s.cfg.DeviceID {
			continue
		}
		a := s.adapters[t.Request.Target.AdapterID]
		if t.State == "QUEUED" && time.Since(s.lastSeen) < 75*time.Second && a.FreeSlots > 0 && a.Health == "ready" && contains(a.Models, t.Request.Target.ModelID) && (len(a.Operations) == 0 || contains(a.Operations, t.Request.Operation)) {
			t.State = "DISPATCHED"
			t.AttemptID = randomID("attempt_")
			t.FenceToken = 1
			t.UpdatedAt = now
		}
		if t.State == "DISPATCHED" {
			c := protocol.Command{Protocol: protocol.Version, Type: "task.dispatch", DeviceID: s.cfg.DeviceID, TaskID: t.ID, AttemptID: t.AttemptID, FenceToken: t.FenceToken, ExpiresAt: t.ExpiresAt, Request: t.Request, Digest: t.Digest}
			c.Signature = s.sign(c)
			return &c
		}
		if t.State == "CANCEL_REQUESTED" {
			c := protocol.Command{Protocol: protocol.Version, Type: "task.cancel", DeviceID: s.cfg.DeviceID, TaskID: t.ID, AttemptID: t.AttemptID, FenceToken: t.FenceToken, ExpiresAt: now.Add(time.Minute)}
			c.Signature = s.sign(c)
			return &c
		}
	}
	return nil
}
func (s *Server) commands(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, true) || !method(w, r, "GET") {
		return
	}
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	for {
		s.wsMu.Lock()
		if s.active != nil {
			s.wsMu.Unlock()
			http.Error(w, "WebSocket session active", http.StatusConflict)
			return
		}
		s.mu.Lock()
		if err := s.expireLocked(); err != nil {
			s.mu.Unlock()
			s.wsMu.Unlock()
			http.Error(w, "storage error", 503)
			return
		}
		if err := s.expireLeasesLocked(); err != nil {
			s.mu.Unlock()
			s.wsMu.Unlock()
			http.Error(w, "storage error", 503)
			return
		}
		cmd := s.nextCommand()
		if err := s.persist(); err != nil {
			s.mu.Unlock()
			s.wsMu.Unlock()
			http.Error(w, "storage error", 503)
			return
		}
		ch := s.changed
		s.mu.Unlock()
		s.wsMu.Unlock()
		if cmd != nil {
			reply(w, 200, cmd)
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			reply(w, 200, map[string]any{"type": "noop"})
			return
		case <-r.Context().Done():
			return
		}
	}
}

type ref struct {
	TaskID     string `json:"task_id"`
	AttemptID  string `json:"attempt_id"`
	FenceToken int64  `json:"fence_token"`
	Digest     string `json:"payload_hash,omitempty"`
}

func (s *Server) find(ref ref) (*record, error) {
	rec := s.state.Tasks[ref.TaskID]
	if rec == nil || rec.Task.AttemptID != ref.AttemptID || rec.Task.FenceToken != ref.FenceToken {
		return nil, errors.New("stale attempt")
	}
	return rec, nil
}
func (s *Server) accepted(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, true) || !method(w, r, "POST") {
		return
	}
	var v ref
	if !decode(w, r, &v, 4096) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.find(v)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	if v.Digest != rec.Task.Digest {
		http.Error(w, "payload conflict", 409)
		return
	}
	if rec.Task.State == "DISPATCHED" {
		before := rec.Task
		rec.Task.State = "ACCEPTED"
		rec.Task.UpdatedAt = time.Now().UTC()
		if err := s.persist(); err != nil {
			rec.Task = before
			http.Error(w, "storage error", 503)
			return
		}
		s.notify()
	}
	reply(w, 200, map[string]any{"state": rec.Task.State})
}
func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, true) || !method(w, r, "POST") {
		return
	}
	var v ref
	if !decode(w, r, &v, 4096) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.find(v)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	if rec.Task.State != "ACCEPTED" || time.Now().After(rec.Task.ExpiresAt) {
		http.Error(w, "start denied", 409)
		return
	}
	if rec.Grant == "" {
		before := *rec
		rec.Grant = randomID("grant_")
		rec.GrantUntil = time.Now().Add(30 * time.Second)
		rec.RunDeadline = time.Now().Add(time.Duration(rec.Task.Request.Options.MaxRuntimeSeconds) * time.Second)
		rec.Task.LeaseUntil = time.Now().Add(120 * time.Second)
		if rec.Task.LeaseUntil.After(rec.RunDeadline) {
			rec.Task.LeaseUntil = rec.RunDeadline
		}
		if err := s.persist(); err != nil {
			*rec = before
			http.Error(w, "storage error", 503)
			return
		}
	}
	reply(w, 200, map[string]any{"grant": rec.Grant, "expires_at": rec.GrantUntil, "lease_until": rec.Task.LeaseUntil})
}

func (s *Server) lease(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, true) || !method(w, r, "POST") {
		return
	}
	var v ref
	if !decode(w, r, &v, 4096) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.expireLeasesLocked(); err != nil {
		http.Error(w, "storage error", 503)
		return
	}
	rec, err := s.find(v)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	if rec.Task.State != "RUNNING" || rec.RunDeadline.IsZero() || !time.Now().Before(rec.RunDeadline) {
		http.Error(w, "lease renewal denied", 409)
		return
	}
	before := rec.Task
	rec.Task.LeaseUntil = time.Now().Add(120 * time.Second)
	if rec.Task.LeaseUntil.After(rec.RunDeadline) {
		rec.Task.LeaseUntil = rec.RunDeadline
	}
	if err := s.persist(); err != nil {
		rec.Task = before
		http.Error(w, "storage error", 503)
		return
	}
	reply(w, 200, map[string]any{"lease_until": rec.Task.LeaseUntil})
}

func allowedKind(k string) bool {
	switch k {
	case "task.started", "output.delta", "task.succeeded", "task.failed", "task.cancelled", "task.unknown":
		return true
	}
	return false
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r, true) || !method(w, r, "POST") {
		return
	}
	var b protocol.EventBatch
	if !decode(w, r, &b, 256<<10) {
		return
	}
	if len(b.Events) == 0 || len(b.Events) > 128 {
		http.Error(w, "invalid event batch", 400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.expireLeasesLocked(); err != nil {
		http.Error(w, "storage error", 503)
		return
	}
	rec, err := s.find(ref{TaskID: b.TaskID, AttemptID: b.AttemptID, FenceToken: b.FenceToken})
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	before := rec.Task
	before.Events = append([]protocol.Event(nil), rec.Task.Events...)
	fail := func(status int, message string) { rec.Task = before; http.Error(w, message, status) }
	for _, ev := range b.Events {
		if !allowedKind(ev.Kind) || len(ev.Data) > 64<<10 {
			fail(422, "invalid event")
			return
		}
		if ev.Seq < 1 {
			fail(409, "invalid event sequence")
			return
		}
		if ev.Seq <= rec.Task.AckSeq {
			old := rec.Task.Events[ev.Seq-1]
			ob, _ := json.Marshal(old)
			nb, _ := json.Marshal(ev)
			if !hmac.Equal(ob, nb) {
				fail(409, "event integrity conflict")
				return
			}
			continue
		}
		if ev.Seq != rec.Task.AckSeq+1 {
			fail(409, "event sequence gap")
			return
		}
		if terminal(rec.Task.State) {
			fail(409, "terminal task")
			return
		}
		if (ev.Kind == "output.delta" || ev.Kind == "task.succeeded") && !rec.Task.LeaseUntil.IsZero() && ev.At.After(rec.Task.LeaseUntil) {
			fail(409, "lease expired before event")
			return
		}
		if ev.Kind == "task.started" {
			if rec.Task.State != "ACCEPTED" || rec.Grant == "" || time.Now().After(rec.GrantUntil) {
				fail(409, "missing or expired start grant")
				return
			}
			rec.Task.State = "RUNNING"
		}
		if ev.Kind == "output.delta" && rec.Task.State != "RUNNING" && rec.Task.State != "CANCEL_REQUESTED" && rec.Task.State != "RECONCILING" {
			fail(409, "output before start")
			return
		}
		if ev.Kind == "task.succeeded" && rec.Task.State != "RUNNING" && rec.Task.State != "CANCEL_REQUESTED" && rec.Task.State != "RECONCILING" {
			fail(409, "success before start")
			return
		}
		if ev.Kind == "task.succeeded" {
			rec.Task.State = "SUCCEEDED"
		}
		if ev.Kind == "task.failed" {
			rec.Task.State = "FAILED"
		}
		if ev.Kind == "task.cancelled" {
			rec.Task.State = "CANCELLED"
		}
		if ev.Kind == "task.unknown" {
			rec.Task.State = "UNKNOWN"
		}
		rec.Task.Events = append(rec.Task.Events, ev)
		rec.Task.AckSeq = ev.Seq
		rec.Task.UpdatedAt = time.Now().UTC()
	}
	if err := s.persist(); err != nil {
		fail(503, "storage error")
		return
	}
	s.notify()
	reply(w, 200, map[string]any{"ack_seq": rec.Task.AckSeq, "state": rec.Task.State})
}

func Serve(cfg Config, addr string) error {
	s, err := New(cfg)
	if err != nil {
		return err
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.mu.Lock()
				err := s.pruneArtifacts()
				s.mu.Unlock()
				if err != nil {
					log.Printf("artifact cleanup: %v", err)
				}
			}
		}
	}()
	log.Printf("remote gateway reference server listening on %s", addr)
	return (&http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}).ListenAndServe()
}
