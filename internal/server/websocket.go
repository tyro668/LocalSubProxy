package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"localsubproxy/internal/protocol"

	"github.com/gorilla/websocket"
)

const maxWSMessage = 256 << 10

type wsSession struct {
	conn  *websocket.Conn
	epoch int64
	write sync.Mutex
	stop  chan struct{}
}

func (s *Server) httpDevice(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.wsMu.Lock()
		defer s.wsMu.Unlock()
		if s.active != nil {
			http.Error(w, "WebSocket session active", http.StatusConflict)
			return
		}
		next(w, r)
	}
}

func (w *wsSession) send(frame protocol.Frame) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(b) > maxWSMessage {
		return errors.New("WebSocket message exceeds 256 KiB")
	}
	w.write.Lock()
	defer w.write.Unlock()
	if err := w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return w.conn.WriteMessage(websocket.TextMessage, b)
}

func wsFrame(kind, id string, epoch int64, payload any) protocol.Frame {
	b, _ := json.Marshal(payload)
	return protocol.Frame{Protocol: protocol.Version, Type: kind, ID: id, SessionEpoch: epoch, Payload: b}
}

func (s *Server) connectWS(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) || !s.auth(w, r, true) {
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "browser origins are not allowed", http.StatusForbidden)
		return
	}
	if !contains(websocket.Subprotocols(r), protocol.Version) {
		http.Error(w, "unsupported subprotocol", http.StatusBadRequest)
		return
	}
	u := websocket.Upgrader{Subprotocols: []string{protocol.Version}, CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := u.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxWSMessage)
	_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(75 * time.Second)) })
	var hello protocol.Frame
	if err := conn.ReadJSON(&hello); err != nil {
		return
	}
	var greeting protocol.Hello
	if hello.Protocol != protocol.Version || hello.Type != "hello" || json.Unmarshal(hello.Payload, &greeting) != nil || greeting.Heartbeat.Protocol != protocol.Version || greeting.Heartbeat.DeviceID != s.cfg.DeviceID || len(greeting.Journal) > 1000 {
		return
	}
	hb := greeting.Heartbeat
	// The persistent epoch fences every write from a superseded connection.
	s.wsMu.Lock()
	s.mu.Lock()
	if s.state.Revoked || (s.cfg.RequireBinding && s.state.OwnerID == "") {
		s.mu.Unlock()
		s.wsMu.Unlock()
		return
	}
	previousEpoch := s.state.SessionEpoch
	s.state.SessionEpoch++
	epoch := s.state.SessionEpoch
	err = s.persist()
	acks := map[string]int64{}
	var attempts []protocol.AttemptState
	if err == nil {
		s.setAdapters(hb)
		s.lastSeen = time.Now()
		s.notify()
		for _, cursor := range greeting.Journal {
			if rec := s.state.Tasks[cursor.TaskID]; rec != nil && rec.Task.AttemptID == cursor.AttemptID {
				acks[cursor.TaskID] = rec.Task.AckSeq
				attempts = append(attempts, protocol.AttemptState{TaskID: rec.Task.ID, AttemptID: rec.Task.AttemptID, State: rec.Task.State, AckSeq: rec.Task.AckSeq, LeaseUntil: rec.Task.LeaseUntil})
			}
		}
	} else {
		s.state.SessionEpoch = previousEpoch
	}
	s.mu.Unlock()
	if err != nil {
		s.wsMu.Unlock()
		return
	}
	old := s.active
	session := &wsSession{conn: conn, epoch: epoch, stop: make(chan struct{})}
	s.active = session
	s.wsMu.Unlock()
	if old != nil {
		_ = old.conn.Close()
	}
	defer func() {
		s.wsMu.Lock()
		if s.active == session {
			s.active = nil
		}
		s.wsMu.Unlock()
	}()
	if err := session.send(wsFrame("welcome", hello.ID, epoch, protocol.Welcome{HeartbeatSeconds: 25, MaxMessageBytes: maxWSMessage, ServerAckSeq: acks, Attempts: attempts})); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { defer close(done); s.pumpWS(session) }()
	defer func() { close(session.stop); _ = conn.Close(); <-done }()
	for {
		var frame protocol.Frame
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		s.wsMu.Lock()
		if s.active != session || frame.Protocol != protocol.Version || frame.SessionEpoch != epoch || frame.ID == "" {
			s.wsMu.Unlock()
			return
		}
		response := s.wsRequest(frame)
		s.wsMu.Unlock()
		if err := session.send(response); err != nil {
			return
		}
	}
}

func (s *Server) wsRequest(frame protocol.Frame) protocol.Frame {
	response := wsFrame(frame.Type+".ack", "", frame.SessionEpoch, nil)
	response.ReplyTo = frame.ID
	var handler func(http.ResponseWriter, *http.Request)
	switch frame.Type {
	case "heartbeat":
		handler = s.heartbeat
	case "task.accepted":
		handler = s.accepted
	case "task.start.request":
		handler = s.start
	case "lease.renew":
		handler = s.lease
	case "task.events":
		handler = s.events
	default:
		response.Error = "unsupported message type"
		return response
	}
	// Invoke the same validated, persistent transition used by HTTPS fallback.
	// The session gate above spans the transition, so a new epoch cannot race it.
	path := map[string]string{"heartbeat": "heartbeat", "task.accepted": "accepted", "task.start.request": "start", "lease.renew": "lease", "task.events": "events"}[frame.Type]
	req := httptest.NewRequest(http.MethodPost, "/v1/connector/"+path, bytes.NewReader(frame.Payload))
	req.Header.Set("Authorization", "Bearer "+s.cfg.DeviceToken)
	recorder := httptest.NewRecorder()
	handler(recorder, req)
	result := recorder.Result()
	defer result.Body.Close()
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		response.Error = fmt.Sprintf("server status %d: %s", result.StatusCode, strings.TrimSpace(recorder.Body.String()))
		return response
	}
	response.Payload = json.RawMessage(bytes.TrimSpace(recorder.Body.Bytes()))
	return response
}

func (s *Server) pumpWS(session *wsSession) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		s.wsMu.Lock()
		if s.active != session {
			s.wsMu.Unlock()
			return
		}
		s.mu.Lock()
		if err := s.expireLocked(); err != nil {
			s.mu.Unlock()
			s.wsMu.Unlock()
			_ = session.conn.Close()
			return
		}
		if err := s.expireLeasesLocked(); err != nil {
			s.mu.Unlock()
			s.wsMu.Unlock()
			_ = session.conn.Close()
			return
		}
		cmd := s.nextCommand()
		if cmd != nil {
			if err := s.persist(); err != nil {
				s.mu.Unlock()
				s.wsMu.Unlock()
				_ = session.conn.Close()
				return
			}
		}
		ch := s.changed
		s.mu.Unlock()
		s.wsMu.Unlock()
		if cmd != nil {
			if err := session.send(wsFrame(cmd.Type, "", session.epoch, cmd)); err != nil {
				_ = session.conn.Close()
				return
			}
		}
		select {
		case <-ch:
		case <-ticker.C:
		case <-session.stop:
			return
		}
	}
}
