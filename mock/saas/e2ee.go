package saas

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"localsubproxy/internal/lsp2"
)

// e2eeChat is the tenant back end side of end-to-end encryption
// (docs/protocol-v2.md §12): it verifies the device's X25519 key statement
// against the jkt pinned at pairing, seals the request as a JWE to that key,
// sends only the routing metadata in clear, and opens each sealed result
// frame before streaming plain OpenAI chunks to the browser. The gateway
// sees ciphertext only.
func (s *Server) e2eeChat(w http.ResponseWriter, r *http.Request, user string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Model       string          `json:"model"`
		Messages    []lsp2.Message  `json:"messages"`
		Tools       []lsp2.Tool     `json:"tools,omitempty"`
		ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
		MaxTokens   int             `json:"max_tokens,omitempty"`
		Temperature *float64        `json:"temperature,omitempty"`
		Stream      bool            `json:"stream"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 40<<20)).Decode(&req); err != nil || len(req.Messages) == 0 {
		apiErr(w, 400, lsp2.ErrBadRequest, "model and messages required")
		return
	}
	dev, model, ok := strings.Cut(req.Model, "/")
	if !ok || !strings.HasPrefix(dev, "dev_") {
		apiErr(w, 400, lsp2.ErrBadRequest, "E2EE requires an explicit device_id/model")
		return
	}
	s.mu.Lock()
	pinned := s.pins[user][dev]
	s.mu.Unlock()
	if pinned == "" {
		apiErr(w, 409, lsp2.ErrForbidden, "device key not pinned (pair this device through the link page)")
		return
	}
	xpub, kid, err := s.deviceE2EEKey(r, user, dev, pinned)
	if err != nil {
		apiErr(w, 409, lsp2.ErrDecrypt, err.Error())
		return
	}
	respKey := make([]byte, 32)
	_, _ = rand.Read(respKey)
	rid := lsp2.RandomID("rid_")
	pt, _ := json.Marshal(lsp2.E2EERequest{RID: rid, Iat: time.Now().Unix(), Model: model, Messages: req.Messages, Tools: req.Tools, ToolChoice: req.ToolChoice,
		MaxTokens: req.MaxTokens, Temperature: req.Temperature, ResponseKey: lsp2.B64(respKey)})
	jwe, err := lsp2.SealJWE(xpub, kid, pt)
	if err != nil {
		apiErr(w, 500, lsp2.ErrInternal, "sealing failed")
		return
	}
	body, _ := json.Marshal(map[string]any{"model": req.Model, "stream": true, "lsp2_e2ee": jwe})
	resp, err := s.gw(r, "POST", "/v1/chat/completions", bytes.NewReader(body), user)
	if err != nil {
		apiErr(w, 502, lsp2.ErrDeviceOffline, "gateway unreachable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("X-Task-Id", resp.Header.Get("X-Task-Id"))
	if resp.StatusCode != 200 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = bufio.NewReader(resp.Body).WriteTo(w)
		return
	}
	created := time.Now().Unix()
	var text strings.Builder
	var calls []lsp2.ToolCall
	finish := "stop"
	var usage *lsp2.Usage
	var failure map[string]any
	emit := func(m map[string]any) {
		if !req.Stream {
			return
		}
		m["id"], m["object"], m["created"], m["model"] = "chatcmpl-"+resp.Header.Get("X-Task-Id"), "chat.completion.chunk", created, req.Model
		b, _ := json.Marshal(m)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		p, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || p == "[DONE]" {
			continue
		}
		var ch struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *lsp2.Usage `json:"usage"`
			E2EE  *struct {
				Seq  int64  `json:"seq"`
				Type string `json:"type"`
				Data string `json:"data"`
			} `json:"lsp2_e2ee"`
			Error map[string]any `json:"error"`
		}
		if json.Unmarshal([]byte(p), &ch) != nil {
			continue
		}
		if ch.Error != nil {
			failure = ch.Error
			emit(map[string]any{"error": ch.Error})
			break
		}
		if ch.E2EE == nil {
			continue
		}
		plain, err := lsp2.OpenChunk(respKey, rid, ch.E2EE.Type, ch.E2EE.Seq, ch.E2EE.Data)
		if err != nil {
			failure = map[string]any{"code": lsp2.ErrDecrypt, "message": "result frame failed authentication"}
			emit(map[string]any{"error": failure})
			break
		}
		if ch.E2EE.Type == lsp2.TypeTaskDelta {
			var d lsp2.TaskDelta
			_ = json.Unmarshal(plain, &d)
			text.WriteString(d.Text)
			calls = mergeCalls(calls, d.ToolCalls)
			delta := map[string]any{}
			if d.Text != "" {
				delta["content"] = d.Text
			}
			if len(d.ToolCalls) > 0 {
				delta["tool_calls"] = d.ToolCalls
			}
			emit(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
			continue
		}
		var dn struct {
			FinishReason string `json:"finish_reason"`
		}
		_ = json.Unmarshal(plain, &dn)
		finish, usage = dn.FinishReason, ch.Usage // sealed finish_reason is authoritative
		emit(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": usage})
		if req.Stream {
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}
	if req.Stream {
		return
	}
	if failure != nil {
		apiErr(w, 502, fmt.Sprint(failure["code"]), fmt.Sprint(failure["message"]))
		return
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-" + resp.Header.Get("X-Task-Id"), "object": "chat.completion", "created": created, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": usage})
}

// deviceE2EEKey fetches the device listing and verifies the key statement
// against the device key pinned at pairing.
func (s *Server) deviceE2EEKey(r *http.Request, user, dev, pinned string) ([]byte, string, error) {
	resp, err := s.gw(r, "GET", "/v2/tenant/devices", nil, user)
	if err != nil {
		return nil, "", fmt.Errorf("gateway unreachable")
	}
	defer resp.Body.Close()
	var list struct {
		Devices []struct {
			ID        string   `json:"device_id"`
			PublicKey lsp2.JWK `json:"public_key"`
			E2EEKey   string   `json:"e2ee_key"`
		} `json:"devices"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	for _, d := range list.Devices {
		if d.ID != dev {
			continue
		}
		pub, err := lsp2.UnB64(d.PublicKey.X)
		if err != nil || d.E2EEKey == "" {
			return nil, "", fmt.Errorf("device has no E2EE key")
		}
		return lsp2.VerifyE2EEKey(pub, pinned, dev, d.E2EEKey, time.Now())
	}
	return nil, "", fmt.Errorf("device not found")
}

func mergeCalls(calls []lsp2.ToolCall, deltas []lsp2.ToolCall) []lsp2.ToolCall {
	for _, c := range deltas {
		i := 0
		if c.Index != nil {
			i = *c.Index
		}
		for len(calls) <= i {
			calls = append(calls, lsp2.ToolCall{Type: "function"})
		}
		if c.ID != "" {
			calls[i].ID = c.ID
		}
		calls[i].Function.Name += c.Function.Name
		calls[i].Function.Arguments += c.Function.Arguments
	}
	return calls
}

func apiErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
