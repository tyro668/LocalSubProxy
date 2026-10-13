package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"localsubproxy/internal/connector"
	"localsubproxy/internal/durable"
	"localsubproxy/internal/lsp2/client"
)

type localIdentity struct {
	JournalKey    string `json:"journal_key"`
	RemoteURL     string `json:"remote_url,omitempty"`
	LegacySaaSURL string `json:"saas_url,omitempty"`
	GatewayURL    string `json:"gateway_url,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
	DeviceToken   string `json:"device_token,omitempty"`
	// Protocol is "v2" (default for new pairings) or "v1" (legacy, opt-in).
	Protocol   string `json:"protocol,omitempty"`
	V2DeviceID string `json:"v2_device_id,omitempty"` // display only; the key lives in device_key.enc
}

const v2KeyFile = "device_key.enc"

// v1Enabled reports whether legacy connector.v1 pairing is allowed
// (LSP_Start {"protocol":"v1"} or LOCALSUBPROXY_PROTOCOL=v1).
func v1Enabled() bool { return runtime.protocol == "v1" }

// loadV2 decrypts the lsp2 identity (Ed25519 private key + pinned gateway key)
// with the per-install key from identity.json.
func loadV2(dir string, id localIdentity) (*client.Identity, error) {
	if id.V2DeviceID == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, v2KeyFile))
	if err != nil {
		return nil, err
	}
	c, err := durable.NewCipher(id.JournalKey)
	if err != nil {
		return nil, err
	}
	var v client.Identity
	if err := c.Open(b, &v); err != nil || !v.Valid() {
		return nil, errors.New("device key file is corrupt or encrypted with another key; pair again")
	}
	return &v, nil
}

func saveV2(dir string, id localIdentity, v client.Identity) error {
	c, err := durable.NewCipher(id.JournalKey)
	if err != nil {
		return err
	}
	b, err := c.Seal(v)
	if err != nil {
		return err
	}
	return durable.Write(filepath.Join(dir, v2KeyFile), b)
}

func (id *localIdentity) normalize() bool {
	if id.LegacySaaSURL == "" {
		return false
	}
	if id.RemoteURL == "" {
		id.RemoteURL = id.LegacySaaSURL
	}
	id.LegacySaaSURL = ""
	return true
}

var runtime struct {
	sync.Mutex
	core   *connector.Connector
	cancel context.CancelFunc
	done   chan struct{}
	dir    string
	local  localIdentity
	login  *connector.CodexLogin
	// lsp2 pairing in progress
	protocol   string
	pairing    *client.Pairing
	pairCancel context.CancelFunc
	pairState  string // idle | pending | paired | failed
	pairErr    string
}

func result(data any, err error) *C.char {
	payload := map[string]any{"ok": err == nil}
	if err != nil {
		payload["error"] = err.Error()
	} else if data != nil {
		payload["data"] = data
	}
	encoded, _ := json.Marshal(payload)
	return C.CString(string(encoded))
}

func coreConfig(dir string, id localIdentity) (connector.Config, error) {
	cfg := connector.Config{ServerURL: id.GatewayURL, RemoteURL: id.RemoteURL, DeviceID: id.DeviceID, DeviceToken: id.DeviceToken,
		JournalKey: id.JournalKey, JournalFile: filepath.Join(dir, "journal.enc"), AdapterFile: filepath.Join(dir, "adapters.enc")}
	if id.Protocol == "v2" {
		v, err := loadV2(dir, id)
		if err != nil {
			return cfg, err
		}
		cfg.ServerURL, cfg.DeviceID, cfg.DeviceToken, cfg.V2 = "", "", "", v
	}
	return cfg, nil
}

func newCore(dir string, id localIdentity) (*connector.Connector, error) {
	cfg, err := coreConfig(dir, id)
	if err != nil {
		return nil, err
	}
	return connector.New(cfg)
}

func pairedLocked() bool {
	return runtime.local.DeviceToken != "" || (runtime.local.Protocol == "v2" && runtime.local.V2DeviceID != "")
}

func statusLocked() map[string]any {
	s := runtime.core.Status()
	s["remote_url"] = runtime.local.RemoteURL
	s["paired"] = pairedLocked()
	s["pairing_state"] = runtime.pairState
	s["pairing_error"] = runtime.pairErr
	s["v1_enabled"] = v1Enabled()
	return s
}

func startRunLocked(core *connector.Connector) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	runtime.cancel, runtime.done = cancel, done
	go func() { _ = core.Run(ctx); close(done) }()
}

//export LSP_Start
func LSP_Start(raw *C.char) *C.char {
	var input struct {
		DataDir    string `json:"data_dir"`
		JournalKey string `json:"journal_key"`
		Protocol   string `json:"protocol"`
	}
	if raw != nil && C.GoString(raw) != "" {
		if err := json.Unmarshal([]byte(C.GoString(raw)), &input); err != nil {
			return result(nil, err)
		}
	}
	dir := input.DataDir
	if dir == "" {
		userDir, err := os.UserConfigDir()
		if err != nil {
			return result(nil, err)
		}
		dir = filepath.Join(userDir, "localsubproxy")
	}
	runtime.Lock()
	defer runtime.Unlock()
	if runtime.core != nil {
		return result(statusLocked(), nil)
	}
	runtime.protocol = "v2"
	if input.Protocol == "v1" || os.Getenv("LOCALSUBPROXY_PROTOCOL") == "v1" {
		runtime.protocol = "v1"
	}
	runtime.pairState = "idle"
	var id localIdentity
	identityFile := filepath.Join(dir, "identity.json")
	if err := durable.ReadJSON(identityFile, &id); err != nil {
		return result(nil, err)
	}
	legacyIdentity := id.normalize()
	migrating := false
	if id.JournalKey == "" {
		if input.JournalKey != "" {
			id.JournalKey = input.JournalKey
			migrating = true
		} else {
			for _, name := range []string{"adapters.enc", "journal.enc"} {
				_, err := os.Stat(filepath.Join(dir, name))
				if err == nil {
					return result(nil, errors.New("legacy encrypted data requires journal key"))
				}
				if !errors.Is(err, os.ErrNotExist) {
					return result(nil, err)
				}
			}
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return result(nil, err)
			}
			id.JournalKey = hex.EncodeToString(key)
			if err := durable.WriteJSON(identityFile, id); err != nil {
				return result(nil, err)
			}
		}
	}
	if id.DeviceToken != "" && id.Protocol == "" {
		id.Protocol = "v1" // existing v1 pairing keeps working only with v1 enabled
	}
	runID := id
	if id.Protocol == "v1" && runtime.protocol != "v1" {
		// v1 is opt-in now: the old credentials stay on disk but are not used.
		runID.GatewayURL, runID.DeviceID, runID.DeviceToken, runID.Protocol = "", "", "", ""
		runtime.pairErr = "旧版 v1 连接已停用，请重新配对（或以 LOCALSUBPROXY_PROTOCOL=v1 启动）"
	}
	core, err := newCore(dir, runID)
	if err != nil {
		return result(nil, err)
	}
	if migrating || legacyIdentity {
		if err := durable.WriteJSON(identityFile, id); err != nil {
			return result(nil, err)
		}
	}
	runtime.core, runtime.dir, runtime.local = core, dir, runID
	id = runID
	if (id.GatewayURL != "" && id.DeviceToken != "") || (id.Protocol == "v2" && id.V2DeviceID != "") {
		startRunLocked(core)
	}
	return result(statusLocked(), nil)
}

// finishPairing runs in the background until the user approves the code on the remote service.
func finishPairing(ctx context.Context, p *client.Pairing, remoteURL string) {
	v, err := p.Wait(ctx)
	runtime.Lock()
	defer runtime.Unlock()
	if runtime.pairing != p {
		return // cancelled or superseded
	}
	runtime.pairing, runtime.pairCancel = nil, nil
	if err != nil {
		runtime.pairState, runtime.pairErr = "failed", err.Error()
		return
	}
	id := runtime.local
	id.Protocol, id.RemoteURL, id.V2DeviceID = "v2", remoteURL, v.DeviceID
	id.GatewayURL, id.DeviceID, id.DeviceToken = "", "", ""
	if err := saveV2(runtime.dir, id, v); err != nil {
		runtime.pairState, runtime.pairErr = "failed", err.Error()
		return
	}
	if err := durable.WriteJSON(filepath.Join(runtime.dir, "identity.json"), id); err != nil {
		runtime.pairState, runtime.pairErr = "failed", err.Error()
		return
	}
	core, err := newCore(runtime.dir, id)
	if err != nil {
		runtime.pairState, runtime.pairErr = "failed", err.Error()
		return
	}
	if runtime.cancel != nil {
		runtime.cancel()
		<-runtime.done
	}
	runtime.core, runtime.local = core, id
	runtime.pairState, runtime.pairErr = "paired", ""
	startRunLocked(core)
}

func cancelPairingLocked() {
	if runtime.pairCancel != nil {
		runtime.pairCancel()
	}
	runtime.pairing, runtime.pairCancel = nil, nil
	if runtime.pairState == "pending" {
		runtime.pairState = "idle"
	}
}

//export LSP_Call
func LSP_Call(raw *C.char) *C.char {
	if raw == nil {
		return result(nil, errors.New("request required"))
	}
	var request struct {
		Action        string                  `json:"action"`
		Adapter       connector.AdapterConfig `json:"adapter"`
		ID            string                  `json:"id"`
		Path          string                  `json:"path"`
		RemoteURL     string                  `json:"remote_url"`
		LegacySaaSURL string                  `json:"saas_url"`
		ShortCode     string                  `json:"short_code"`
	}
	if err := json.Unmarshal([]byte(C.GoString(raw)), &request); err != nil {
		return result(nil, err)
	}
	remoteURL := request.RemoteURL
	if remoteURL == "" {
		remoteURL = request.LegacySaaSURL
	}
	runtime.Lock()
	defer runtime.Unlock()
	if request.Action == "stop" {
		cancelPairingLocked()
		if runtime.login != nil {
			runtime.login.Close()
			runtime.login = nil
		}
		if runtime.cancel != nil {
			runtime.cancel()
			<-runtime.done
		}
		runtime.core, runtime.cancel, runtime.done = nil, nil, nil
		return result(nil, nil)
	}
	if runtime.core == nil {
		return result(nil, errors.New("local kernel not running"))
	}
	switch request.Action {
	case "status":
		return result(statusLocked(), nil)
	case "list_adapters":
		return result(map[string]any{"adapters": runtime.core.ListAdapters()}, nil)
	case "create_adapter":
		a, err := runtime.core.CreateAdapter(request.Adapter)
		return result(a, err)
	case "update_adapter":
		a, err := runtime.core.UpdateAdapter(request.Adapter)
		return result(a, err)
	case "delete_adapter":
		return result(nil, runtime.core.DeleteAdapter(request.ID))
	case "test_adapter":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		health, err := runtime.core.TestAdapter(ctx, request.Adapter)
		return result(health, err)
	case "codex_models":
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		models, err := connector.CodexModels(ctx, request.Path)
		return result(models, err)
	case "codex_login_start":
		if runtime.login != nil {
			runtime.login.Close()
			runtime.login = nil
		}
		login, err := connector.BeginCodexLogin(request.Path)
		if err != nil {
			return result(nil, err)
		}
		runtime.login = login
		return result(map[string]string{"auth_url": login.AuthURL}, nil)
	case "codex_login_check":
		if runtime.login == nil {
			return result(nil, errors.New("no Codex login in progress"))
		}
		models, complete, err := runtime.login.Check()
		if complete {
			runtime.login = nil
		}
		return result(map[string]any{"complete": complete, "models": models}, err)
	case "codex_login_cancel":
		if runtime.login != nil {
			runtime.login.Close()
			runtime.login = nil
		}
		return result(nil, nil)
	case "codex_logout":
		if runtime.login != nil {
			runtime.login.Close()
			runtime.login = nil
		}
		return result(nil, connector.LogoutCodex(request.Path))
	case "pair_start":
		if pairedLocked() {
			return result(nil, errors.New("already paired; disconnect first"))
		}
		cancelPairingLocked()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		p, err := client.StartPairing(ctx, nil, remoteURL, connector.DeviceName())
		cancel()
		if err != nil {
			return result(nil, err)
		}
		wctx, wcancel := context.WithCancel(context.Background())
		runtime.pairing, runtime.pairCancel, runtime.pairState, runtime.pairErr = p, wcancel, "pending", ""
		go finishPairing(wctx, p, remoteURL)
		return result(map[string]any{"user_code": p.UserCode, "link_url": p.LinkURL, "expires_at": p.ExpiresAt}, nil)
	case "pair_status":
		out := map[string]any{"state": runtime.pairState, "error": runtime.pairErr}
		if runtime.pairing != nil {
			out["user_code"], out["link_url"], out["expires_at"] = runtime.pairing.UserCode, runtime.pairing.LinkURL, runtime.pairing.ExpiresAt
		}
		if runtime.pairState == "paired" {
			out["status"] = statusLocked()
		}
		return result(out, nil)
	case "pair_cancel":
		cancelPairingLocked()
		return result(nil, nil)
	case "prepare_connection":
		if !v1Enabled() {
			return result(nil, errors.New("v1 pairing is disabled; use pair_start (protocol v2)"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		return result(nil, connector.CheckRemoteService(ctx, remoteURL))
	case "claim":
		if !v1Enabled() {
			return result(nil, errors.New("v1 pairing is disabled; use pair_start (protocol v2)"))
		}
		if runtime.local.DeviceToken != "" {
			return result(nil, errors.New("already paired; disconnect first"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
		defer cancel()
		claim, err := connector.ClaimRemoteService(ctx, remoteURL, request.ShortCode)
		if err != nil {
			return result(nil, err)
		}
		id := runtime.local
		id.RemoteURL, id.GatewayURL, id.DeviceID, id.DeviceToken, id.Protocol = remoteURL, claim.GatewayURL, claim.DeviceID, claim.DeviceToken, "v1"
		core, err := newCore(runtime.dir, id)
		if err != nil {
			return result(nil, err)
		}
		if err := durable.WriteJSON(filepath.Join(runtime.dir, "identity.json"), id); err != nil {
			return result(nil, err)
		}
		runtime.core, runtime.local = core, id
		startRunLocked(core)
		return result(statusLocked(), nil)
	case "disconnect":
		if runtime.core.Status()["running_tasks"].(int) > 0 {
			return result(nil, errors.New("tasks are still running"))
		}
		if runtime.cancel != nil {
			runtime.cancel()
			<-runtime.done
			runtime.cancel, runtime.done = nil, nil
		}
		cancelPairingLocked()
		id := runtime.local
		id.RemoteURL, id.GatewayURL, id.DeviceID, id.DeviceToken, id.Protocol, id.V2DeviceID = "", "", "", "", "", ""
		_ = os.Remove(filepath.Join(runtime.dir, v2KeyFile)) // drop the device private key; re-pairing generates a new one
		core, err := newCore(runtime.dir, id)
		if err != nil {
			return result(nil, err)
		}
		if err := durable.WriteJSON(filepath.Join(runtime.dir, "identity.json"), id); err != nil {
			return result(nil, err)
		}
		runtime.core, runtime.local = core, id
		return result(statusLocked(), nil)
	default:
		return result(nil, errors.New("unknown action"))
	}
}

//export LSP_Free
func LSP_Free(ptr unsafe.Pointer) {
	if ptr != nil {
		C.free(ptr)
	}
}

func main() {}
