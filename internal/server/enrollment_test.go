package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"localsubproxy/internal/protocol"
)

func TestPairingBindsOwnerAndPersists(t *testing.T) {
	cfg := Config{DataFile: filepath.Join(t.TempDir(), "server.json"), RemoteToken: "remote-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc", RequireBinding: true}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := devicePost(s, "/v1/connector/heartbeat", protocol.Heartbeat{Protocol: protocol.Version, DeviceID: cfg.DeviceID}); got.Code != 403 {
		t.Fatalf("unbound heartbeat: %d", got.Code)
	}
	makeRequest := func(path string, body any, token string) *httptest.ResponseRecorder {
		var payload []byte
		if body != nil {
			payload, _ = json.Marshal(body)
		}
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if got := makeRequest("/v1/connector/pairing", nil, cfg.RemoteToken); got.Code != 401 {
		t.Fatalf("remote service token issued code: %d", got.Code)
	}
	issued := makeRequest("/v1/connector/pairing", nil, cfg.DeviceToken)
	if issued.Code != 200 {
		t.Fatalf("issue: %d %s", issued.Code, issued.Body.String())
	}
	var code struct {
		ShortCode string `json:"short_code"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &code); err != nil || len(code.ShortCode) != 8 {
		t.Fatalf("code: %q %v", code.ShortCode, err)
	}
	stored, err := os.ReadFile(cfg.DataFile)
	if err != nil || bytes.Contains(stored, []byte(code.ShortCode)) {
		t.Fatal("pairing code stored in plaintext")
	}
	path := "/v1/connectors/dev_1/bind"
	wrong := "0" + code.ShortCode[1:]
	if code.ShortCode[0] == '0' {
		wrong = "1" + code.ShortCode[1:]
	}
	if got := makeRequest(path, map[string]string{"user_id": "user_demo-user", "short_code": wrong}, cfg.RemoteToken); got.Code != 409 {
		t.Fatalf("wrong code: %d", got.Code)
	}
	if s.state.PairingAttempts != 1 {
		t.Fatal("failed attempt was not counted")
	}
	for i := 0; i < 4; i++ {
		_ = makeRequest(path, map[string]string{"user_id": "user_demo-user", "short_code": wrong}, cfg.RemoteToken)
	}
	if got := makeRequest(path, map[string]string{"user_id": "user_demo-user", "short_code": code.ShortCode}, cfg.RemoteToken); got.Code != 409 {
		t.Fatalf("locked code accepted: %d", got.Code)
	}
	issued = makeRequest("/v1/connector/pairing", nil, cfg.DeviceToken)
	if issued.Code != 200 {
		t.Fatalf("reissue: %d", issued.Code)
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &code); err != nil {
		t.Fatal(err)
	}
	if got := makeRequest(path, map[string]string{"user_id": "user_demo-user", "short_code": code.ShortCode}, cfg.DeviceToken); got.Code != 401 {
		t.Fatalf("device bound itself: %d", got.Code)
	}
	if got := makeRequest(path, map[string]string{"user_id": "user_demo-user", "short_code": code.ShortCode}, cfg.RemoteToken); got.Code != 200 {
		t.Fatalf("bind: %d %s", got.Code, got.Body.String())
	}
	if s.state.OwnerID != "user_demo-user" || s.state.PairingHash != "" {
		t.Fatal("binding state invalid")
	}
	if got := devicePost(s, "/v1/connector/heartbeat", protocol.Heartbeat{Protocol: protocol.Version, DeviceID: cfg.DeviceID}); got.Code != 200 {
		t.Fatalf("bound heartbeat: %d", got.Code)
	}
	if got := makeRequest(path, map[string]string{"user_id": "other", "short_code": code.ShortCode}, cfg.RemoteToken); got.Code != 409 {
		t.Fatalf("code reuse: %d", got.Code)
	}
	reopened, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.state.OwnerID != "user_demo-user" || !strings.Contains(string(stored), "pairing_hash") {
		t.Fatal("binding was not persisted")
	}
}

func TestRemoteServiceIssuedCodeClaimsLocalConnector(t *testing.T) {
	cfg := Config{DataFile: filepath.Join(t.TempDir(), "server.json"), RemoteToken: "remote-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc", RequireBinding: true}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string, body any, token string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	issuePath := "/v1/connectors/dev_1/pairing-code"
	if got := request(issuePath, map[string]string{"user_id": "user_demo"}, ""); got.Code != 401 {
		t.Fatalf("unauthorized issue: %d", got.Code)
	}
	issued := request(issuePath, map[string]string{"user_id": "user_demo"}, cfg.RemoteToken)
	if issued.Code != 200 {
		t.Fatalf("issue: %d %s", issued.Code, issued.Body.String())
	}
	var code struct {
		ShortCode string `json:"short_code"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &code); err != nil || len(code.ShortCode) != 8 {
		t.Fatalf("invalid code: %q %v", code.ShortCode, err)
	}
	if got := request("/v1/connectors/dev_1/bind", map[string]string{"user_id": "user_demo", "short_code": code.ShortCode}, cfg.RemoteToken); got.Code != 409 {
		t.Fatalf("remote service code accepted by legacy bind route: %d", got.Code)
	}
	stored, err := os.ReadFile(cfg.DataFile)
	if err != nil || bytes.Contains(stored, []byte(code.ShortCode)) {
		t.Fatal("pairing code persisted in plaintext")
	}
	wrong := "0" + code.ShortCode[1:]
	if wrong == code.ShortCode {
		wrong = "1" + code.ShortCode[1:]
	}
	if got := request("/v1/connector/claim", map[string]string{"device_id": "dev_1", "short_code": wrong}, ""); got.Code != 409 {
		t.Fatalf("wrong code: %d", got.Code)
	}
	for i := 0; i < 4; i++ {
		_ = request("/v1/connector/claim", map[string]string{"device_id": "dev_1", "short_code": wrong}, "")
	}
	if got := request("/v1/connector/claim", map[string]string{"device_id": "dev_1", "short_code": code.ShortCode}, ""); got.Code != 409 {
		t.Fatalf("code accepted after five failures: %d", got.Code)
	}
	issued = request(issuePath, map[string]string{"user_id": "user_demo"}, cfg.RemoteToken)
	if issued.Code != 200 {
		t.Fatalf("reissue: %d", issued.Code)
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &code); err != nil {
		t.Fatal(err)
	}
	claim := request("/v1/connector/claim", map[string]string{"device_id": "dev_1", "short_code": code.ShortCode}, "")
	if claim.Code != 200 {
		t.Fatalf("claim: %d %s", claim.Code, claim.Body.String())
	}
	var result struct {
		DeviceToken string `json:"device_token"`
		OwnerID     string `json:"owner_id"`
	}
	if err := json.Unmarshal(claim.Body.Bytes(), &result); err != nil || result.DeviceToken != cfg.DeviceToken || result.OwnerID != "user_demo" {
		t.Fatalf("claim result: %+v %v", result, err)
	}
	if got := request("/v1/connector/claim", map[string]string{"device_id": "dev_1", "short_code": code.ShortCode}, ""); got.Code != 409 {
		t.Fatalf("reused code: %d", got.Code)
	}
	if got := devicePost(s, "/v1/connector/heartbeat", protocol.Heartbeat{Protocol: protocol.Version, DeviceID: cfg.DeviceID}); got.Code != 200 {
		t.Fatalf("bound heartbeat: %d", got.Code)
	}
	reopened, err := New(cfg)
	if err != nil || reopened.state.OwnerID != "user_demo" || reopened.state.PairingHash != "" {
		t.Fatalf("persistent claim: %v", err)
	}
	reissued := request(issuePath, map[string]string{"user_id": "user_demo"}, cfg.RemoteToken)
	if reissued.Code != 200 {
		t.Fatalf("same owner could not reconnect: %d", reissued.Code)
	}
	if err := json.Unmarshal(reissued.Body.Bytes(), &code); err != nil {
		t.Fatal(err)
	}
	if got := request("/v1/connector/claim", map[string]string{"device_id": "dev_1", "short_code": code.ShortCode}, ""); got.Code != 200 {
		t.Fatalf("same owner reclaim: %d", got.Code)
	}
	if got := request(issuePath, map[string]string{"user_id": "another_owner"}, cfg.RemoteToken); got.Code != 403 {
		t.Fatalf("another owner issued code: %d", got.Code)
	}
}
