// Package conformance is a language-agnostic black-box test suite for lsp2
// gateways. It plays the device (connector) and the tenant back end against
// any gateway URL and reports which normative requirements hold. Run it via
// cmd/lsp2-conformance.
package conformance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"localsubproxy/internal/lsp2"
)

type Config struct {
	GatewayURL string // http(s)://host[:port]
	Tenant     string
	TenantKey  string
	EndUser    string
	SchemaDir  string // spec/schemas; empty disables frame validation
	Timeout    time.Duration
	Log        io.Writer
}

type Result struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

type runner struct {
	cfg     Config
	hc      *http.Client
	results []Result
	schema  *jsonschema.Schema
	payload *jsonschema.Schema
	gwKey   ed25519.PublicKey
	devKey  ed25519.PrivateKey
	devID   string
}

var errStop = errors.New("prerequisite failed")

func (r *runner) check(id, title string, err error) bool {
	res := Result{ID: id, Title: title, Pass: err == nil}
	if err != nil {
		res.Detail = err.Error()
	}
	r.results = append(r.results, res)
	if r.cfg.Log != nil {
		mark := "PASS"
		if !res.Pass {
			mark = "FAIL"
		}
		fmt.Fprintf(r.cfg.Log, "%s  %-6s %s %s\n", mark, id, title, res.Detail)
	}
	return res.Pass
}

// Run executes the suite; it never panics on a misbehaving gateway.
func Run(ctx context.Context, cfg Config) []Result {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.EndUser == "" {
		cfg.EndUser = "conformance-user"
	}
	cfg.GatewayURL = strings.TrimRight(cfg.GatewayURL, "/")
	r := &runner{cfg: cfg, hc: &http.Client{Timeout: cfg.Timeout}}
	if cfg.SchemaDir != "" {
		var err error
		r.schema, r.payload, err = loadSchemas(cfg.SchemaDir)
		if !r.check("S0", "spec/schemas compile (JSON Schema 2020-12)", err) {
			return r.results
		}
	}
	_ = r.run(ctx)
	return r.results
}

func loadSchemas(dir string) (*jsonschema.Schema, *jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, nil, err
		}
		if err := c.AddResource("https://localsubproxy.dev/spec/lsp2/"+e.Name(), doc); err != nil {
			return nil, nil, err
		}
	}
	env, err := c.Compile("https://localsubproxy.dev/spec/lsp2/envelope.json")
	if err != nil {
		return nil, nil, err
	}
	p, err := c.Compile("https://localsubproxy.dev/spec/lsp2/task.run.payload.json")
	return env, p, err
}

func (r *runner) validate(s *jsonschema.Schema, raw []byte) error {
	if s == nil {
		return nil
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return s.Validate(v)
}

func (r *runner) form(path string, f url.Values) (int, map[string]any, http.Header, error) {
	req, _ := http.NewRequest(http.MethodPost, r.cfg.GatewayURL+path, strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r.do(req)
}

func (r *runner) tenant(method, path string, body any) (int, map[string]any, http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, r.cfg.GatewayURL+path, rd)
	req.Header.Set("Authorization", "Bearer "+r.cfg.TenantKey)
	req.Header.Set("X-End-User", r.cfg.EndUser)
	req.Header.Set("Content-Type", "application/json")
	return r.do(req)
}

func (r *runner) do(req *http.Request) (int, map[string]any, http.Header, error) {
	resp, err := r.hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	var m map[string]any
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m, resp.Header, nil
}

func want(cond bool, f string, a ...any) error {
	if cond {
		return nil
	}
	return fmt.Errorf(f, a...)
}

var userCodeRe = regexp.MustCompile(`^[B-DF-HJ-NP-TV-XZ]{4}-[B-DF-HJ-NP-TV-XZ]{4}$`)

func (r *runner) run(ctx context.Context) error {
	// --- discovery
	st, jwks, h, err := r.tenant(http.MethodGet, "/.well-known/jwks.json", nil)
	var jwkX string
	if err == nil && st == 200 {
		if ks, _ := jwks["keys"].([]any); len(ks) > 0 {
			k, _ := ks[0].(map[string]any)
			if k["kty"] == "OKP" && k["crv"] == "Ed25519" {
				jwkX, _ = k["x"].(string)
			}
		}
	}
	_ = h
	r.check("D1", "GET /.well-known/jwks.json returns an OKP/Ed25519 JWK Set", want(err == nil && jwkX != "", "status %d err %v", st, err))

	// --- pairing (RFC 8628)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	r.devKey = key
	st, auth, h, err := r.form("/v2/device/authorize", url.Values{"tenant": {r.cfg.Tenant}, "public_key": {lsp2.B64(pub)}, "device_name": {"lsp2-conformance"}})
	dc, _ := auth["device_code"].(string)
	uc, _ := auth["user_code"].(string)
	interval, _ := auth["interval"].(float64)
	if !r.check("P1", "device authorization (form-encoded) returns device_code/user_code/expires_in/interval",
		want(err == nil && st == 200 && dc != "" && uc != "" && auth["expires_in"] != nil && interval >= 1, "status %d body %v err %v", st, auth, err)) {
		return errStop
	}
	r.check("P2", "user_code matches XXXX-XXXX over BCDFGHJKLMNPQRSTVWXZ", want(userCodeRe.MatchString(uc), "got %q", uc))
	r.check("P3", "device authorization response has Cache-Control: no-store", want(strings.Contains(h.Get("Cache-Control"), "no-store"), "got %q", h.Get("Cache-Control")))
	tok := func(proof string) (int, map[string]any) {
		st, m, _, _ := r.form("/v2/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {dc}, "proof": {proof}})
		return st, m
	}
	proof := func() string { return lsp2.SignProof(key, "", lsp2.ProofToken, dc, time.Now()) }
	_, m := tok(proof())
	r.check("P4", "polling before approval yields authorization_pending or slow_down", want(m["error"] == "authorization_pending" || m["error"] == "slow_down", "got %v", m))
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	_, m = tok(lsp2.SignProof(other, "", lsp2.ProofToken, dc, time.Now()))
	r.check("P5", "token request with a proof from a different key is rejected (invalid_grant)", want(m["error"] == "invalid_grant", "got %v", m))
	st, _, _, _ = r.tenant(http.MethodPost, "/v2/tenant/device-approvals", map[string]any{"user_code": "BBBB-BBBB"})
	r.check("P6", "approving an unknown user_code fails with 404", want(st == 404, "status %d", st))
	st, ap, _, err := r.tenant(http.MethodPost, "/v2/tenant/device-approvals", map[string]any{"user_code": strings.ToLower(strings.ReplaceAll(uc, "-", ""))})
	if !r.check("P7", "tenant back end approves the code (case/separator-insensitive)", want(err == nil && st == 200 && ap["status"] == "approved", "status %d body %v", st, ap)) {
		return errStop
	}
	var tr map[string]any
	deadline := time.Now().Add(r.cfg.Timeout + time.Duration(interval)*time.Second*3)
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(interval)*time.Second + 100*time.Millisecond)
		var s int
		s, tr = tok(proof())
		if s == 200 {
			break
		}
		if tr["error"] == "slow_down" {
			interval += 5
		} else if tr["error"] != "authorization_pending" {
			break
		}
	}
	at, _ := tr["access_token"].(string)
	r.devID, _ = tr["device_id"].(string)
	gk, _ := tr["gateway_key"].(map[string]any)
	x, _ := gk["x"].(string)
	gb, _ := lsp2.UnB64(x)
	r.gwKey = gb
	if !r.check("T1", "device_code grant returns access_token, token_type Bearer, device_id, gateway_key JWK", want(at != "" && tr["token_type"] == "Bearer" && r.devID != "" && len(gb) == 32, "body %v", tr)) {
		return errStop
	}
	r.check("T2", "gateway_key equals the JWKS key", want(jwkX == "" || jwkX == x, "jwks %s token %s", jwkX, x))
	claims, err := lsp2.VerifyJWT(r.gwKey, at, lsp2.TypAccessToken, "lsp2-gateway", time.Now())
	if err != nil { // aud is deployment specific; retry reading it from the token
		if c, e2 := lsp2.UnverifiedClaims(at); e2 == nil {
			claims, err = lsp2.VerifyJWT(r.gwKey, at, lsp2.TypAccessToken, c.Aud, time.Now())
		}
	}
	r.check("T3", "access token is an EdDSA JWT with typ at+jwt signed by gateway_key", err)
	r.check("T4", "access token sub = device_id and cnf.jkt = RFC 7638 thumbprint of the device key",
		want(claims.Sub == r.devID && claims.Cnf["jkt"] == lsp2.Thumbprint(pub), "sub %q jkt %q", claims.Sub, claims.Cnf["jkt"]))
	_, m = tok(proof())
	r.check("T5", "device_code is single-use", want(m["error"] == "invalid_grant", "got %v", m))
	assertion := func() string {
		now := time.Now()
		return lsp2.SignJWT(key, lsp2.TypAssertion, lsp2.Thumbprint(pub), lsp2.Claims{Iss: r.devID, Sub: r.devID, Aud: claims.Aud + "/token", Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Jti: lsp2.RandomID("a_")})
	}
	a1 := assertion()
	refresh := func(a string) (int, map[string]any) {
		st, m, _, _ := r.form("/v2/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {a}})
		return st, m
	}
	st, m = refresh(a1)
	r.check("T6", "jwt-bearer grant with a device-signed assertion issues a new token", want(st == 200 && m["access_token"] != nil, "status %d %v", st, m))
	_, m = refresh(a1)
	r.check("T7", "replayed assertion (same jti) is rejected", want(m["error"] == "invalid_grant", "got %v", m))

	// --- WebSocket
	wsURL := "ws" + strings.TrimPrefix(r.cfg.GatewayURL, "http") + "/v2/connect"
	_, resp, err := (&websocket.Dialer{HandshakeTimeout: r.cfg.Timeout}).Dial(wsURL, http.Header{"Authorization": {"Bearer " + at}})
	r.check("W1", "upgrade without subprotocol lsp2 is refused", want(err != nil && resp != nil && resp.StatusCode >= 400, "connected or no HTTP error"))
	d := websocket.Dialer{Subprotocols: []string{lsp2.Subprotocol}, HandshakeTimeout: r.cfg.Timeout}
	_, resp, err = d.Dial(wsURL, http.Header{"Authorization": {"Bearer " + at + "x"}})
	r.check("W2", "upgrade with an invalid access token is refused with 401", want(err != nil && resp != nil && resp.StatusCode == 401, "resp %v", status(resp)))
	conn, resp, err := d.Dial(wsURL, http.Header{"Authorization": {"Bearer " + at}})
	if err == nil {
		_ = r.writeFrame(conn, "hello", "h1", "", lsp2.Hello{DeviceID: r.devID, Client: "lsp2-conformance", Models: []string{"m"}, MaxConcurrency: 1,
			Proof: lsp2.SignProof(key, r.devID, lsp2.ProofHello, "some-other-token", time.Now())})
		f, _ := r.readFrame(conn)
		r.check("W3", "hello whose proof is bound to another token is rejected (error/close, no welcome)", want(f.Type != lsp2.TypeWelcome, "got welcome"))
		conn.Close()
	} else {
		r.check("W3", "hello whose proof is bound to another token is rejected", fmt.Errorf("could not connect: %v %s", err, status(resp)))
	}
	conn, resp, err = d.Dial(wsURL, http.Header{"Authorization": {"Bearer " + at}})
	if !r.check("W4", "upgrade with valid token selects subprotocol lsp2", want(err == nil && conn.Subprotocol() == lsp2.Subprotocol, "err %v %s", err, status(resp))) {
		return errStop
	}
	defer conn.Close()
	model := "conformance-model"
	_ = r.writeFrame(conn, "hello", "h1", "", lsp2.Hello{DeviceID: r.devID, Client: "lsp2-conformance", Models: []string{model}, MaxConcurrency: 2,
		Capabilities: []string{"session.token", "models.update", "x-unknown-cap"}, Proof: lsp2.SignProof(key, r.devID, lsp2.ProofHello, at, time.Now())})
	f, err := r.readFrame(conn)
	var wel lsp2.Welcome
	_ = json.Unmarshal(f.Data, &wel)
	if !r.check("W5", "valid hello is answered by welcome with the same id", want(err == nil && f.Type == lsp2.TypeWelcome && f.ID == "h1" && wel.SessionID != "", "frame %+v err %v", f, err)) {
		return errStop
	}
	r.check("W6", "welcome.capabilities ⊆ offered capabilities (unknown ones dropped)", want(!containsS(wel.Capabilities, "x-unknown-cap"), "got %v", wel.Capabilities))
	_ = r.writeFrame(conn, "x.vendor.unknown", "u1", "", map[string]any{"a": 1})
	_ = r.writeFrame(conn, "ping", "p1", "", nil)
	var gotPong bool
	for i := 0; i < 3 && !gotPong; i++ {
		f, err = r.readFrame(conn)
		gotPong = err == nil && f.Type == lsp2.TypePong && f.ID == "p1"
	}
	r.check("W7", "unknown message type does not kill the session; ping is answered by pong with same id", want(gotPong, "last frame %+v err %v", f, err))

	// models visible through the OpenAI surface
	st, ml, _, _ := r.tenant(http.MethodGet, "/v1/models", nil)
	full := r.devID + "/" + model
	found := false
	if data, _ := ml["data"].([]any); data != nil {
		for _, it := range data {
			if mm, _ := it.(map[string]any); mm["id"] == full {
				found = true
			}
		}
	}
	r.check("O1", "GET /v1/models lists <device_id>/<model> for the owner", want(st == 200 && found, "status %d %v", st, ml))

	// streaming task
	frames := make(chan lsp2.Envelope, 64)
	go func() {
		defer close(frames)
		for {
			f, err := r.readFrame(conn)
			if err != nil {
				return
			}
			frames <- f
		}
	}()
	sse := make(chan []string, 1)
	go func() { sse <- r.chat(ctx, full, true) }()
	run, err := r.awaitRun(frames)
	r.check("R1", "task.run arrives with no data member and a jws whose typ is lsp2-task+jws", err)
	if err == nil {
		r.check("R2", "task.run JWS verifies with gateway_key; payload task_id/device_id/model/exp are correct",
			want(run.DeviceID == r.devID && run.Model == model && run.Exp > time.Now().Unix() && len(run.Nonce) >= 16, "payload %+v", run))
		for i, t := range []string{"Hel", "lo"} {
			_ = r.writeFrame(conn, "task.delta", "", run.TaskID, lsp2.TaskDelta{Text: t}, int64(i+1))
		}
		_ = r.writeFrame(conn, "task.done", "", run.TaskID, lsp2.TaskDone{FinishReason: "stop", Usage: &lsp2.Usage{PromptTokens: 1, CompletionTokens: 2}}, 3)
		lines := <-sse
		joined := strings.Join(lines, "\n")
		r.check("O2", "SSE stream relays deltas as chat.completion.chunk and ends with data: [DONE]",
			want(strings.Contains(joined, `"content":"Hel"`) && strings.Contains(joined, `"content":"lo"`) && len(lines) > 0 && lines[len(lines)-1] == "data: [DONE]", "got %q", joined))
	}
	// cancellation
	cctx, ccancel := context.WithCancel(ctx)
	go func() { r.chat(cctx, full, true) }()
	run2, err := r.awaitRun(frames)
	if err == nil {
		_ = r.writeFrame(conn, "task.delta", "", run2.TaskID, lsp2.TaskDelta{Text: "x"}, 1)
		time.Sleep(200 * time.Millisecond)
		ccancel()
		got := false
		tm := time.After(r.cfg.Timeout)
	wait:
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					break wait
				}
				if f.Type == lsp2.TypeTaskCancel && f.TaskID == run2.TaskID {
					got = true
					break wait
				}
			case <-tm:
				break wait
			}
		}
		r.check("C1", "client disconnect mid-stream produces task.cancel for that task_id", want(got, "no task.cancel"))
	} else {
		ccancel()
		r.check("C1", "client disconnect mid-stream produces task.cancel", err)
	}
	// session.token
	st, m = refresh(assertion())
	newTok, _ := m["access_token"].(string)
	_ = r.writeFrame(conn, "session.token", "t1", "", lsp2.SessionToken{AccessToken: newTok, Proof: lsp2.SignProof(key, r.devID, lsp2.ProofSessionToken, newTok, time.Now())})
	got := false
	tm := time.After(r.cfg.Timeout)
loop:
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				break loop
			}
			var stt lsp2.SessionToken
			_ = json.Unmarshal(f.Data, &stt)
			if f.Type == lsp2.TypeSessionToken && f.ID == "t1" && stt.ExpiresAt > 0 {
				got = true
				break loop
			}
		case <-tm:
			break loop
		}
	}
	r.check("K1", "session.token with a fresh token + proof is acknowledged with token_expires_at", want(st == 200 && got, "no acknowledgement"))
	// revoke
	st, _, _, _ = r.tenant(http.MethodPost, "/v2/tenant/devices/"+url.PathEscape(r.devID)+"/revoke", nil)
	closed := false
	tm = time.After(r.cfg.Timeout)
	for !closed {
		select {
		case _, ok := <-frames:
			closed = !ok
		case <-tm:
			goto done
		}
	}
done:
	r.check("V1", "revoking the device closes its live session", want(st == 200 && closed, "status %d closed %v", st, closed))
	st, m = refresh(assertion())
	r.check("V2", "revoked device cannot obtain new tokens", want(m["error"] == "invalid_grant", "status %d %v", st, m))
	return nil
}

func (r *runner) awaitRun(frames <-chan lsp2.Envelope) (lsp2.TaskRun, error) {
	tm := time.After(r.cfg.Timeout)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return lsp2.TaskRun{}, errors.New("session closed")
			}
			if f.Type != lsp2.TypeTaskRun {
				continue
			}
			if len(f.Data) > 0 {
				return lsp2.TaskRun{}, errors.New("task.run carries data")
			}
			parts := strings.Split(f.JWS, ".")
			if len(parts) == 3 {
				if p, err := lsp2.UnB64(parts[1]); err == nil {
					if err := r.validate(r.payload, p); err != nil {
						return lsp2.TaskRun{}, fmt.Errorf("payload schema: %v", err)
					}
				}
			}
			return lsp2.VerifyTaskRun(r.gwKey, f.TaskID, f.JWS)
		case <-tm:
			return lsp2.TaskRun{}, errors.New("timeout waiting for task.run")
		}
	}
}

func (r *runner) chat(ctx context.Context, model string, stream bool) []string {
	b, _ := json.Marshal(map[string]any{"model": model, "stream": stream, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.GatewayURL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+r.cfg.TenantKey)
	req.Header.Set("X-End-User", r.cfg.EndUser)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if l := sc.Text(); strings.HasPrefix(l, "data: ") {
			lines = append(lines, l)
		}
	}
	return lines
}

func (r *runner) writeFrame(c *websocket.Conn, typ, id, taskID string, data any, seq ...int64) error {
	e := lsp2.Envelope{V: lsp2.Version, Type: typ, ID: id, TaskID: taskID}
	if len(seq) > 0 {
		e.Seq = seq[0]
	}
	if data != nil {
		e.Data, _ = json.Marshal(data)
	}
	_ = c.SetWriteDeadline(time.Now().Add(r.cfg.Timeout))
	return c.WriteJSON(e)
}

// readFrame reads one text frame and validates it against the envelope schema.
func (r *runner) readFrame(c *websocket.Conn) (lsp2.Envelope, error) {
	var e lsp2.Envelope
	_ = c.SetReadDeadline(time.Now().Add(r.cfg.Timeout + 30*time.Second))
	mt, b, err := c.ReadMessage()
	if err != nil {
		return e, err
	}
	if mt != websocket.TextMessage {
		r.check("F0", "gateway sends only text frames", errors.New("binary frame"))
	}
	if err := r.validate(r.schema, b); err != nil {
		r.check("F1", "gateway frame matches envelope.json", fmt.Errorf("%s: %v", b, err))
	}
	return e, json.Unmarshal(b, &e)
}

func status(r *http.Response) string {
	if r == nil {
		return "no response"
	}
	return r.Status
}

func containsS(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
