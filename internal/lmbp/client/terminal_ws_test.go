package client_test

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tyro668/avora/internal/lmbp"
	"github.com/tyro668/avora/internal/lmbp/client"
	"github.com/tyro668/avora/internal/lmbp/gateway"
)

// ackProxy relays the device WebSocket to the real gateway and can hold
// the gateway's task.ack frames (so the device's flow credit runs out).
type ackProxy struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hold bool
	held [][]byte
	dev  *websocket.Conn
	wmu  sync.Mutex
}

func newAckProxy(t *testing.T, gwURL string) *ackProxy {
	p := &ackProxy{hold: true}
	u, _ := url.Parse(gwURL)
	rp := httputil.NewSingleHostReverseProxy(u)
	od := rp.Director
	rp.Director = func(r *http.Request) { od(r); r.Host = u.Host } // the gateway derives its issuer from Host
	up := websocket.Upgrader{Subprotocols: []string{lmbp.Subprotocol}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			rp.ServeHTTP(w, r)
			return
		}
		d := websocket.Dialer{Subprotocols: []string{lmbp.Subprotocol}}
		back, _, err := d.Dial("ws"+strings.TrimPrefix(gwURL, "http")+r.URL.Path, http.Header{"Authorization": r.Header["Authorization"]})
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		dev, err := up.Upgrade(w, r, nil)
		if err != nil {
			back.Close()
			return
		}
		p.mu.Lock()
		p.dev = dev
		p.mu.Unlock()
		go func() { // device -> gateway
			defer back.Close()
			for {
				mt, b, err := dev.ReadMessage()
				if err != nil || back.WriteMessage(mt, b) != nil {
					return
				}
			}
		}()
		go func() { // gateway -> device
			defer dev.Close()
			for {
				mt, b, err := back.ReadMessage()
				if err != nil {
					return
				}
				var e lmbp.Envelope
				_ = json.Unmarshal(b, &e)
				p.mu.Lock()
				if p.hold && e.Type == lmbp.TypeTaskAck {
					p.held = append(p.held, b)
					p.mu.Unlock()
					continue
				}
				p.mu.Unlock()
				p.wmu.Lock()
				err = dev.WriteMessage(mt, b)
				p.wmu.Unlock()
				if err != nil {
					return
				}
			}
		}()
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *ackProxy) release() {
	p.mu.Lock()
	held, dev := p.held, p.dev
	p.held, p.hold = nil, false
	p.mu.Unlock()
	p.wmu.Lock()
	defer p.wmu.Unlock()
	for _, b := range held {
		_ = dev.WriteMessage(websocket.TextMessage, b)
	}
}

// floodExec streams model "a" until the flow window is full; model "b" is
// advertised at first and then withdrawn locally, so a task.run for it is
// rejected immediately by the read loop.
type floodExec struct{ bGone atomic.Bool }

func (f *floodExec) Models() []string {
	if f.bGone.Load() {
		return []string{"a"}
	}
	return []string{"a", "b"}
}

func (f *floodExec) Execute(ctx context.Context, run lmbp.TaskRun, emit func(lmbp.TaskDelta) error) (*lmbp.TaskDone, *lmbp.ErrorBody) {
	chunk := strings.Repeat("x", 8<<10)
	for i := 0; i < 64 && ctx.Err() == nil; i++ { // 512 KiB > the 256 KiB window
		if emit(lmbp.TaskDelta{Text: chunk}) != nil {
			break
		}
	}
	return &lmbp.TaskDone{FinishReason: "stop"}, nil
}

// Review6 P1-A: with the flow credit exhausted, the read loop rejects a
// task.run immediately. The terminal frame must not wait for credit in the
// read loop (the ACKs that free it arrive on that loop): after the ACKs
// come in, the reject reaches the gateway; and a Run cancel while credit is
// still exhausted returns promptly.
func TestImmediateRejectWithExhaustedCredit(t *testing.T) {
	for _, mode := range []string{"ack", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			g := newGWCfg(t, ctx, func(c *gateway.Config) {
				c.Flow = lmbp.Flow{TaskWindow: lmbp.MaxFrameBytes, ConnWindow: lmbp.MaxFrameBytes}
			})
			id := g.id
			px := newAckProxy(t, id.GatewayURL)
			id.GatewayURL = px.srv.URL
			ex := &floodExec{}
			c, err := client.New(client.Config{Executor: ex, MaxConcurrency: 4, Logger: log.New(os.Stderr, "", 0)}, id) //LOG
			if err != nil {
				t.Fatal(err)
			}
			rctx, rcancel := context.WithCancel(ctx)
			defer rcancel()
			ready, ran := make(chan error, 1), make(chan struct{})
			go func() { _ = c.Run(rctx, ready); close(ran) }()
			select {
			case err := <-ready:
				if err != nil {
					t.Fatal(err)
				}
			case <-ran:
				t.Fatal("Run ended before the session was ready")
			}
			chat := func(model string) <-chan string {
				out := make(chan string, 1)
				go func() {
					body, _ := g.try("POST", "/v1/chat/completions", map[string]any{"model": id.DeviceID + "/" + model, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
					out <- body
				}()
				return out
			}
			_ = chat("a")
			for c.OutboxWaiting() == 0 { // task a is blocked on credit
				select {
				case <-ctx.Done():
					t.Fatal("credit never ran out")
				case <-time.After(10 * time.Millisecond):
				}
			}
			ex.bGone.Store(true)
			b := chat("b")              // rejected by the read loop
			for c.OutboxWaiting() < 2 { // its task.error waits for credit too
				select {
				case <-ctx.Done():
					t.Fatal("reject never queued")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if mode == "cancel" {
				rcancel()
				select {
				case <-ran:
				case <-time.After(10 * time.Second):
					t.Fatal("Run did not return after cancel (read loop blocked on credit)")
				}
				return
			}
			px.release()
			select {
			case body := <-b:
				if !strings.Contains(body, "error") {
					t.Fatalf("task b: %s", body)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("reject never reached the gateway (read loop deadlocked)")
			}
			rcancel()
			select {
			case <-ran:
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return")
			}
		})
	}
}
