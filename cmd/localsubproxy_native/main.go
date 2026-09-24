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
)

type localIdentity struct {
	JournalKey    string `json:"journal_key"`
	RemoteURL     string `json:"remote_url,omitempty"`
	LegacySaaSURL string `json:"saas_url,omitempty"`
	GatewayURL    string `json:"gateway_url,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
	DeviceToken   string `json:"device_token,omitempty"`
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

func coreConfig(dir string, id localIdentity) connector.Config {
	return connector.Config{ServerURL: id.GatewayURL, RemoteURL: id.RemoteURL, DeviceID: id.DeviceID, DeviceToken: id.DeviceToken,
		JournalKey: id.JournalKey, JournalFile: filepath.Join(dir, "journal.enc"), AdapterFile: filepath.Join(dir, "adapters.enc")}
}

func statusLocked() map[string]any {
	s := runtime.core.Status()
	s["remote_url"] = runtime.local.RemoteURL
	s["paired"] = runtime.local.DeviceToken != ""
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
	core, err := connector.New(coreConfig(dir, id))
	if err != nil {
		return result(nil, err)
	}
	if migrating || legacyIdentity {
		if err := durable.WriteJSON(identityFile, id); err != nil {
			return result(nil, err)
		}
	}
	runtime.core, runtime.dir, runtime.local = core, dir, id
	if id.GatewayURL != "" && id.DeviceToken != "" {
		startRunLocked(core)
	}
	return result(statusLocked(), nil)
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
	case "prepare_connection":
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		return result(nil, connector.CheckRemoteService(ctx, remoteURL))
	case "claim":
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
		id.RemoteURL, id.GatewayURL, id.DeviceID, id.DeviceToken = remoteURL, claim.GatewayURL, claim.DeviceID, claim.DeviceToken
		core, err := connector.New(coreConfig(runtime.dir, id))
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
		id := runtime.local
		id.RemoteURL, id.GatewayURL, id.DeviceID, id.DeviceToken = "", "", "", ""
		core, err := connector.New(coreConfig(runtime.dir, id))
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
