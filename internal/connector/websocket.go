package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"localsubproxy/internal/protocol"

	"github.com/gorilla/websocket"
)

const maxWSMessage = 256 << 10

type wsClient struct {
	conn     *websocket.Conn
	epoch    int64
	write    sync.Mutex
	mu       sync.Mutex
	pending  map[string]chan protocol.Frame
	commands chan protocol.Command
	done     chan struct{}
	nextID   atomic.Uint64
}

func (w *wsClient) send(frame protocol.Frame) error {
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

func (w *wsClient) read() {
	defer close(w.done)
	defer w.conn.Close()
	w.conn.SetReadLimit(maxWSMessage)
	_ = w.conn.SetReadDeadline(time.Now().Add(75 * time.Second))
	w.conn.SetPongHandler(func(string) error { return w.conn.SetReadDeadline(time.Now().Add(75 * time.Second)) })
	for {
		var frame protocol.Frame
		if err := w.conn.ReadJSON(&frame); err != nil {
			return
		}
		if frame.Protocol != protocol.Version || frame.SessionEpoch != w.epoch {
			return
		}
		_ = w.conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		if frame.ReplyTo != "" {
			w.mu.Lock()
			ch := w.pending[frame.ReplyTo]
			delete(w.pending, frame.ReplyTo)
			w.mu.Unlock()
			if ch != nil {
				ch <- frame
			}
			continue
		}
		if frame.Type != "task.dispatch" && frame.Type != "task.cancel" {
			return
		}
		var cmd protocol.Command
		if err := json.Unmarshal(frame.Payload, &cmd); err != nil || cmd.Type != frame.Type {
			return
		}
		select {
		case w.commands <- cmd:
		case <-w.done:
			return
		default:
			return // Bounded inbound queue; reconnect and replay.
		}
	}
}

func (w *wsClient) call(ctx context.Context, kind string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("req_%d", w.nextID.Add(1))
	ch := make(chan protocol.Frame, 1)
	w.mu.Lock()
	w.pending[id] = ch
	w.mu.Unlock()
	defer func() { w.mu.Lock(); delete(w.pending, id); w.mu.Unlock() }()
	if err := w.send(protocol.Frame{Protocol: protocol.Version, Type: kind, ID: id, SessionEpoch: w.epoch, Payload: b}); err != nil {
		_ = w.conn.Close()
		return err
	}
	select {
	case frame := <-ch:
		if frame.Error != "" {
			return errors.New(frame.Error)
		}
		if out != nil {
			return json.Unmarshal(frame.Payload, out)
		}
		return nil
	case <-w.done:
		return errors.New("WebSocket disconnected")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (x *Connector) currentWS() *wsClient {
	x.wsMu.Lock()
	defer x.wsMu.Unlock()
	return x.ws
}

func (x *Connector) runWS(ctx context.Context) error {
	u, _ := url.Parse(x.cfg.ServerURL)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/v1/connector/connect"
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 10 * time.Second, Subprotocols: []string{protocol.Version}}
	if x.wsDialer != nil {
		dialer = *x.wsDialer
	}
	if !containsString(dialer.Subprotocols, protocol.Version) {
		dialer.Subprotocols = []string{protocol.Version}
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+x.cfg.DeviceToken)
	conn, resp, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("WebSocket handshake: %s: %w", resp.Status, err)
		}
		return err
	}
	defer conn.Close()
	stopOnCancel := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopOnCancel:
		}
	}()
	defer close(stopOnCancel)
	if conn.Subprotocol() != protocol.Version {
		return errors.New("WebSocket subprotocol mismatch")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	hb := x.probeAll(probeCtx)
	cancel()
	x.mu.Lock()
	var cursors []protocol.ResumeCursor
	for _, task := range x.j.Tasks {
		if !isTerminal(task.State) || task.AckSeq < int64(len(task.Events)) {
			cursors = append(cursors, protocol.ResumeCursor{TaskID: task.Command.TaskID, AttemptID: task.Command.AttemptID, State: task.State, AckSeq: task.AckSeq, StartIntent: task.StartIntent})
		}
	}
	x.mu.Unlock()
	b, _ := json.Marshal(protocol.Hello{Heartbeat: hb, Journal: cursors})
	if err := conn.WriteJSON(protocol.Frame{Protocol: protocol.Version, Type: "hello", ID: "hello", Payload: b}); err != nil {
		return err
	}
	conn.SetReadLimit(maxWSMessage)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var welcome protocol.Frame
	if err := conn.ReadJSON(&welcome); err != nil {
		return err
	}
	if welcome.Protocol != protocol.Version || welcome.Type != "welcome" || welcome.ID != "hello" || welcome.SessionEpoch <= 0 {
		return errors.New("invalid WebSocket welcome")
	}
	var settings protocol.Welcome
	if err := json.Unmarshal(welcome.Payload, &settings); err != nil || settings.HeartbeatSeconds != 25 || settings.MaxMessageBytes != maxWSMessage {
		return errors.New("unsupported WebSocket session settings")
	}
	w := &wsClient{conn: conn, epoch: welcome.SessionEpoch, pending: map[string]chan protocol.Frame{}, commands: make(chan protocol.Command, 16), done: make(chan struct{})}
	x.wsMu.Lock()
	x.ws = w
	x.wsMu.Unlock()
	go w.read()
	defer func() {
		x.wsMu.Lock()
		if x.ws == w {
			x.ws = nil
		}
		x.wsMu.Unlock()
		_ = conn.Close()
		<-w.done
	}()
	if err := x.resumeAccepted(ctx); err != nil {
		return err
	}
	if err := x.sendPending(ctx); err != nil {
		return err
	}
	for {
		select {
		case cmd := <-w.commands:
			if err := x.handle(ctx, cmd); err != nil {
				// Replayed commands can be stale; keep the transport alive.
				log.Printf("command rejected: %v", err)
			}
		case <-w.done:
			return errors.New("WebSocket disconnected")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func wsKind(path string) string {
	switch strings.TrimPrefix(path, "/v1/connector/") {
	case "heartbeat":
		return "heartbeat"
	case "accepted":
		return "task.accepted"
	case "start":
		return "task.start.request"
	case "lease":
		return "lease.renew"
	case "events":
		return "task.events"
	}
	return ""
}
