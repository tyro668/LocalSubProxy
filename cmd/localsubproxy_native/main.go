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

	"github.com/tyro668/LocalSubProxy/internal/audit"
	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/durable"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
)

// localIdentity is identity.json: the per-install data key plus display
// fields. The device private keys live encrypted in device_key.enc.
type localIdentity struct {
	DataKey   string `json:"data_key"`
	RemoteURL string `json:"remote_url,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	// JournalKey is the upstream app's name for the same key (dda2be5 sealed
	// adapters.enc with it). It is kept on disk so a downgrade still works,
	// and tried as a legacy key when it differs from DataKey.
	JournalKey string `json:"journal_key,omitempty"`
}

// loadIdentity reads identity.json. An upstream file (journal_key only)
// adopts that key as the data key; a new key is generated only when the
// file has no key at all, and existing fields are never discarded.
func loadIdentity(dir string) (localIdentity, error) {
	var id localIdentity
	f := filepath.Join(dir, "identity.json")
	if err := durable.ReadJSON(f, &id); err != nil {
		return id, err
	}
	switch {
	case id.DataKey != "":
		return id, nil
	case id.JournalKey != "":
		id.DataKey = id.JournalKey
	default:
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return id, err
		}
		id.DataKey = hex.EncodeToString(key)
	}
	return id, durable.WriteJSON(f, id)
}

func (id localIdentity) legacyKeys() []string {
	if id.JournalKey != "" && id.JournalKey != id.DataKey {
		return []string{id.JournalKey}
	}
	return nil
}

const deviceKeyFile = "device_key.enc"

// loadDevice decrypts the lmbp identity (Ed25519 + X25519 private keys and
// the pinned gateway key) with the per-install data key.
func loadDevice(dir string, id localIdentity) (*client.Identity, error) {
	if id.DeviceID == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, deviceKeyFile))
	if err != nil {
		return nil, err
	}
	c, err := durable.NewCipher(id.DataKey)
	if err != nil {
		return nil, err
	}
	var v client.Identity
	if err := c.Open(b, &v); err != nil || !v.Valid() {
		return nil, errors.New("device key file is corrupt or encrypted with another key; pair again")
	}
	return &v, nil
}

func saveDevice(dir string, id localIdentity, v client.Identity) error {
	c, err := durable.NewCipher(id.DataKey)
	if err != nil {
		return err
	}
	b, err := c.Seal(v)
	if err != nil {
		return err
	}
	return durable.Write(filepath.Join(dir, deviceKeyFile), b)
}

var runtime struct {
	sync.Mutex
	core      *connector.Connector
	cancel    context.CancelFunc
	done      chan struct{}
	dir       string
	local     localIdentity
	login     *connector.CodexLogin
	audit     *audit.Log // per-connection request log (audit.db)
	auditStop context.CancelFunc
	// lmbp pairing in progress
	pairing    *client.Pairing
	pairReq    *client.PairRequest // verified, awaiting the user's consent decision
	pairCancel context.CancelFunc
	pairState  string // idle | consent | pending | paired | failed | denied
	pairErr    string
}

func result(data any, err error) string {
	payload := map[string]any{"ok": err == nil}
	if err != nil {
		payload["error"] = err.Error()
	} else if data != nil {
		payload["data"] = data
	}
	encoded, _ := json.Marshal(payload)
	return string(encoded)
}

func newCore(dir string, id localIdentity) (*connector.Connector, error) {
	v, err := loadDevice(dir, id)
	if err != nil {
		return nil, err
	}
	return connector.New(connector.Config{Identity: v, RemoteURL: id.RemoteURL, DataKey: id.DataKey, LegacyDataKeys: id.legacyKeys(), AdapterFile: filepath.Join(dir, "adapters.enc"), Audit: runtime.audit})
}

func pairedLocked() bool { return runtime.local.DeviceID != "" }

func statusLocked() map[string]any {
	s := runtime.core.Status()
	s["remote_url"] = runtime.local.RemoteURL
	s["paired"] = pairedLocked()
	s["pairing_state"] = runtime.pairState
	s["pairing_error"] = runtime.pairErr
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
	in := ""
	if raw != nil {
		in = C.GoString(raw)
	}
	return C.CString(startJSON(in))
}

// startJSON / callJSON are the C API minus cgo, so tests can drive the
// kernel with the exact JSON the Dart UI sends.
func startJSON(raw string) string {
	var input struct {
		DataDir string `json:"data_dir"`
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
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
	runtime.pairState = "idle"
	id, err := loadIdentity(dir)
	if err != nil {
		return result(nil, err)
	}
	if runtime.audit == nil {
		al, err := audit.Open(filepath.Join(dir, "audit.db"))
		if err != nil {
			return result(nil, err)
		}
		actx, acancel := context.WithCancel(context.Background())
		go al.RunRetention(actx, audit.DefaultRetention, time.Hour)
		runtime.audit, runtime.auditStop = al, acancel
	}
	core, err := newCore(dir, id)
	if err != nil {
		return result(nil, err)
	}
	runtime.core, runtime.dir, runtime.local = core, dir, id
	if pairedLocked() {
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
		if client.IsConsentDenied(err) {
			runtime.pairState = "denied"
		}
		return
	}
	id := runtime.local
	id.RemoteURL, id.DeviceID = remoteURL, v.DeviceID
	if err := saveDevice(runtime.dir, id, v); err != nil {
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
	runtime.pairing, runtime.pairCancel, runtime.pairReq = nil, nil, nil
	if runtime.pairState == "pending" || runtime.pairState == "consent" {
		runtime.pairState = "idle"
	}
}

//export LSP_Call
func LSP_Call(raw *C.char) *C.char {
	if raw == nil {
		return C.CString(result(nil, errors.New("request required")))
	}
	return C.CString(callJSON(C.GoString(raw)))
}

func callJSON(raw string) string {
	if raw == "" {
		return result(nil, errors.New("request required"))
	}
	var request struct {
		Action    string                  `json:"action"`
		Adapter   connector.AdapterConfig `json:"adapter"`
		ID        string                  `json:"id"`
		Path      string                  `json:"path"`
		RemoteURL string                  `json:"remote_url"`
		Link      string                  `json:"link"` // lmbp://pair deep link
		Filter    audit.Filter            `json:"filter"`
		GroupBy   []string                `json:"group_by"`
		LocalTime bool                    `json:"local_time"`
		Days      int                     `json:"days"`
	}
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		return result(nil, err)
	}
	remoteURL := request.RemoteURL
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
	case "audit_list":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rows, err := runtime.core.AuditList(ctx, request.Filter)
		return result(map[string]any{"records": rows}, err)
	case "usage_stats":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b, err := runtime.core.UsageStats(ctx, request.Filter, request.GroupBy, request.LocalTime)
		return result(map[string]any{"buckets": b, "group_by": request.GroupBy}, err)
	case "audit_purge":
		if request.Days < 0 {
			return result(nil, errors.New("days must be >= 0"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		n, err := runtime.audit.Purge(ctx, time.Now().Add(-time.Duration(request.Days)*24*time.Hour))
		return result(map[string]any{"removed": n}, err)
	case "list_adapters":
		return result(map[string]any{"adapters": runtime.core.ListAdapters()}, nil)
	case "create_adapter":
		a, err := runtime.core.CreateAdapter(request.Adapter)
		return result(a, err)
	case "update_adapter":
		a, err := runtime.core.UpdateAdapter(request.Adapter)
		return result(a, err)
	case "adapter_recovery_ack":
		return result(nil, runtime.core.AcknowledgeAdapterRecovery())
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
	case "pair_start", "pair_prepare":
		// Step 1: resolve the SaaS (remote_url or lmbp://pair link), verify the
		// gateway's signed metadata and return the consent screen data. Nothing
		// is registered at the gateway until pair_approve.
		if pairedLocked() {
			return result(nil, errors.New("already paired; disconnect first"))
		}
		cancelPairingLocked()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		var req *client.PairRequest
		var err error
		if request.Link != "" {
			req, err = client.PrepareLink(ctx, nil, request.Link)
		} else {
			req, err = client.Prepare(ctx, nil, remoteURL)
		}
		cancel()
		if err != nil {
			return result(nil, err)
		}
		runtime.pairReq, runtime.pairState, runtime.pairErr = req, "consent", ""
		return result(map[string]any{"state": "consent", "consent": req.Consent}, nil)
	case "pair_approve":
		// Step 2a: the user explicitly approved the SaaS shown on the consent screen.
		req := runtime.pairReq
		if req == nil {
			return result(nil, errors.New("no pairing awaiting consent"))
		}
		runtime.pairReq = nil
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		p, err := req.Approve(ctx, connector.DeviceName())
		cancel()
		if err != nil {
			runtime.pairState, runtime.pairErr = "failed", err.Error()
			return result(nil, err)
		}
		wctx, wcancel := context.WithCancel(context.Background())
		remote := req.Consent.Remote
		if remote == "" {
			remote = req.Consent.SaaS.HomepageURI
		}
		runtime.pairing, runtime.pairCancel, runtime.pairState, runtime.pairErr = p, wcancel, "pending", ""
		go finishPairing(wctx, p, remote)
		return result(map[string]any{"user_code": p.UserCode, "link_url": p.LinkURL, "jkt": p.JKT, "expires_at": p.ExpiresAt, "consent": p.Consent}, nil)
	case "pair_deny":
		// Step 2b: the user declined; nothing was sent to the gateway.
		req := runtime.pairReq
		if req == nil {
			return result(nil, errors.New("no pairing awaiting consent"))
		}
		runtime.pairReq = nil
		err := req.Deny()
		runtime.pairState, runtime.pairErr = "denied", err.Error()
		return result(map[string]any{"state": "denied", "code": "CONSENT_DENIED"}, nil)
	case "pair_status":
		out := map[string]any{"state": runtime.pairState, "error": runtime.pairErr}
		if runtime.pairReq != nil {
			out["consent"] = runtime.pairReq.Consent
		}
		if runtime.pairing != nil {
			out["consent"] = runtime.pairing.Consent
			out["user_code"], out["link_url"], out["expires_at"] = runtime.pairing.UserCode, runtime.pairing.LinkURL, runtime.pairing.ExpiresAt
		}
		if runtime.pairState == "paired" {
			out["status"] = statusLocked()
		}
		return result(out, nil)
	case "pair_cancel":
		cancelPairingLocked()
		return result(nil, nil)
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
		id := localIdentity{DataKey: runtime.local.DataKey}
		_ = os.Remove(filepath.Join(runtime.dir, deviceKeyFile)) // drop the device private key; re-pairing generates a new one
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
