package gateway

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

type rawConn struct {
	c      *websocket.Conn
	frames chan lmbp.Envelope
}

// rawHello opens a device connection and sends a hello (optionally resuming).
func (r *rig) rawHello(t *testing.T, resumeToken string) *rawConn {
	t.Helper()
	dev, err := r.g.cfg.Store.GetDevice(context.Background(), r.devID)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := r.g.issue(dev, r.id.Issuer)["access_token"].(string)
	kb, _ := lmbp.UnB64(r.id.PrivateKey)
	key := ed25519.PrivateKey(kb)
	d := websocket.Dialer{Subprotocols: []string{lmbp.Subprotocol}, HandshakeTimeout: 5 * time.Second}
	c, _, err := d.Dial("ws"+strings.TrimPrefix(r.srv.URL, "http")+"/lmbp/v1/connect", http.Header{"Authorization": {"Bearer " + tok}})
	if err != nil {
		t.Fatal(err)
	}
	h := lmbp.Hello{DeviceID: r.devID, Client: "raw", Models: r.exec.Models(), MaxConcurrency: 4,
		Capabilities: []string{lmbp.CapResume, lmbp.CapModelsUpdate}, Proof: lmbp.SignProof(key, r.devID, lmbp.ProofHello, tok, time.Now())}
	if resumeToken != "" {
		h.Resume = &lmbp.HelloResume{Token: resumeToken, Tasks: []lmbp.ResumeTask{}}
	}
	hb, _ := json.Marshal(h)
	if err := c.WriteJSON(lmbp.Envelope{V: lmbp.Version, Type: lmbp.TypeHello, ID: "h1", Data: hb}); err != nil {
		t.Fatal(err)
	}
	rc := &rawConn{c: c, frames: make(chan lmbp.Envelope, 64)}
	go func() {
		defer close(rc.frames)
		for {
			var e lmbp.Envelope
			if c.ReadJSON(&e) != nil {
				return
			}
			rc.frames <- e
		}
	}()
	t.Cleanup(func() { c.Close() })
	return rc
}

// frames collects what arrived on the connection within d.
func (rc *rawConn) collect(d time.Duration) (out []lmbp.Envelope) {
	tm := time.After(d)
	for {
		select {
		case e, ok := <-rc.frames:
			if !ok {
				return
			}
			out = append(out, e)
		case <-tm:
			return
		}
	}
}

// audit3 P2-2 (gateway): two resume handshakes with the same token overlap.
// Each must get at most its own welcome on its own connection, only one may
// win, and the winner's sender is the one installed (tasks reach it).
func TestOverlappingResumeHandshakes(t *testing.T) {
	r := newRig(t, lmbp.Flow{TaskWindow: lmbp.DefaultTaskWindow, ConnWindow: lmbp.DefaultConnWindow}, 0, time.Millisecond)
	r.stop()
	<-r.done
	r0 := r.rawHello(t, "")
	var w0 lmbp.Welcome
	for _, e := range r0.collect(time.Second) {
		if e.Type == lmbp.TypeWelcome {
			_ = json.Unmarshal(e.Data, &w0)
		}
	}
	if w0.ResumeToken == "" {
		t.Fatal("no resume token")
	}
	r0.c.Close()
	time.Sleep(200 * time.Millisecond) // the gateway notices: resume window open
	// both handshakes pass the token check before either activates
	var arrived sync.WaitGroup
	arrived.Add(2)
	var n sync.Mutex
	count := 0
	activateHook = func(string) {
		n.Lock()
		count++
		mine := count
		n.Unlock()
		if mine <= 2 {
			arrived.Done()
			ch := make(chan struct{})
			go func() { arrived.Wait(); close(ch) }()
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
			}
		}
	}
	// ...and the first to activate is slow to send its welcome
	var hk sync.Once
	handshakeHook = func(string) { hk.Do(func() { time.Sleep(300 * time.Millisecond) }) }
	defer func() { activateHook, handshakeHook = nil, nil }()
	a, b := r.rawHello(t, w0.ResumeToken), r.rawHello(t, w0.ResumeToken)
	var fa, fb []lmbp.Envelope
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); fa = a.collect(1500 * time.Millisecond) }()
	go func() { defer wg.Done(); fb = b.collect(1500 * time.Millisecond) }()
	wg.Wait()
	welcomes := func(fs []lmbp.Envelope) int {
		k := 0
		for _, e := range fs {
			if e.Type == lmbp.TypeWelcome {
				k++
			}
		}
		return k
	}
	wa, wb := welcomes(fa), welcomes(fb)
	if wa > 1 || wb > 1 || wa+wb != 1 {
		t.Fatalf("welcomes: a=%d b=%d (want exactly one connection welcomed once)", wa, wb)
	}
	win := a
	if wb == 1 {
		win = b
	}
	// the installed sender belongs to the winner: a task reaches it
	go func() {
		req := r.req("POST", "/v1/chat/completions", map[string]any{"model": r.model(r.exec.Models()[0]), "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
		cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer ccancel()
		req = req.WithContext(cctx)
		if resp, err := httpc.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	got := false
	for _, e := range win.collect(2 * time.Second) {
		if e.Type == lmbp.TypeTaskRun {
			got = true
		}
	}
	if !got {
		t.Fatal("task.run never reached the welcomed connection: a superseded handshake's sender is installed")
	}
}
