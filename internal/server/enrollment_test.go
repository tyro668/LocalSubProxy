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
	cfg := Config{DataFile: filepath.Join(t.TempDir(), "server.json"), SAASToken: "saas-token-0123456789-0123456789-abcd", DeviceID: "dev_1", DeviceToken: "device-token-0123456789-0123456789-abc", RequireBinding: true}
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
	if got := makeRequest("/v1/connector/pairing", nil, cfg.SAASToken); got.Code != 401 {
		t.Fatalf("SaaS token issued code: %d", got.Code)
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
	if got := makeRequest(path, map[string]string{"user_id": "user_demo-user", "short_code": wrong}, cfg.SAASToken); got.Code != 409 {
		t.Fatalf("wrong code: %d", got.Code)
	}
	if s.state.PairingAttempts != 1 {
		t.Fatal("failed attempt was not counted")
	}
	for i := 0; i < 4; i++ {
		_ = makeRequest(path, map[string]string{"user_id": "user_demo-user", "short_code": wrong}, cfg.SAASToken)
	}
	if got := makeRequest(path, map[string]string{"user_id": "user_demo-user", "short_code": code.ShortCode}, cfg.SAASToken); got.Code != 409 {
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
	if got := makeRequest(path, map[string]string{"user_id": "user_demo-user", "short_code": code.ShortCode}, cfg.SAASToken); got.Code != 200 {
		t.Fatalf("bind: %d %s", got.Code, got.Body.String())
	}
	if s.state.OwnerID != "user_demo-user" || s.state.PairingHash != "" {
		t.Fatal("binding state invalid")
	}
	if got := devicePost(s, "/v1/connector/heartbeat", protocol.Heartbeat{Protocol: protocol.Version, DeviceID: cfg.DeviceID}); got.Code != 200 {
		t.Fatalf("bound heartbeat: %d", got.Code)
	}
	if got := makeRequest(path, map[string]string{"user_id": "other", "short_code": code.ShortCode}, cfg.SAASToken); got.Code != 409 {
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
