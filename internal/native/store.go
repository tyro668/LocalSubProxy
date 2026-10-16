package native

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/durable"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
)

// Identity store v2 (identity.json):
//
//	{"version":2,"data_key":..,"journal_key":..,"bindings":[{id,device_id,issuer,tenant,saas_name,remote_url,key_file,paired_at,paused,policy}]}
//
// Each binding (one paired SaaS gateway + tenant) has its own device key
// file bindings/<id>/device_key.enc sealed with data_key. A v1 file (one
// device_id + device_key.enc) is migrated on load; the originals are kept
// as identity.json.v1.bak and device_key.enc.v1.bak.

const storeVersion = 2

// Injected in tests to simulate write and delete failures.
var (
	writeJSON = durable.WriteJSON
	removeAll = os.RemoveAll
	removeOne = os.Remove
	readFile  = os.ReadFile
)

type binding struct {
	ID        string           `json:"id"`
	DeviceID  string           `json:"device_id"`
	Issuer    string           `json:"issuer,omitempty"`
	Tenant    string           `json:"tenant,omitempty"`
	SaaSName  string           `json:"saas_name,omitempty"`
	RemoteURL string           `json:"remote_url,omitempty"` // display origin (consent remote)
	KeyFile   string           `json:"key_file"`             // relative to the data dir
	PairedAt  string           `json:"paired_at,omitempty"`
	Paused    bool             `json:"paused,omitempty"`
	Migrated  bool             `json:"migrated,omitempty"` // came from a v1 identity.json
	Policy    connector.Policy `json:"policy"`
}

func newBindingID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "b_" + hex.EncodeToString(b), nil
}

func bindingKeyFile(id string) string { return filepath.Join("bindings", id, deviceKeyFile) }

func copyIfAbsent(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return durable.Write(dst, b)
}

// migrateV1 turns a v1 single pairing into the first binding. It never
// deletes the only copy of a key: the v1 files are backed up first and the
// new key file is verified before the old one is renamed to its backup.
func migrateV1(dir string, id *localIdentity) error {
	if id.Version >= storeVersion {
		return nil
	}
	f := filepath.Join(dir, "identity.json")
	if id.DeviceID != "" {
		old := filepath.Join(dir, deviceKeyFile)
		raw, rerr := readFile(old)
		missing := errors.Is(rerr, os.ErrNotExist)
		if rerr != nil && !missing {
			// Permission or I/O error: the key may well exist. Touch
			// nothing (the v1 state stays as it is) and retry next start.
			return fmt.Errorf("read v1 device key %s (nothing was changed; fix access and restart): %w", old, rerr)
		}
		if err := copyIfAbsent(f, f+".v1.bak"); err != nil {
			return fmt.Errorf("backup identity.json: %w", err)
		}
		if rerr == nil {
			if err := copyIfAbsent(old, old+".v1.bak"); err != nil {
				return fmt.Errorf("backup device key: %w", err)
			}
		}
		bid, err := newBindingID()
		if err != nil {
			return err
		}
		b := binding{ID: bid, DeviceID: id.DeviceID, RemoteURL: id.RemoteURL, KeyFile: bindingKeyFile(bid), Migrated: true,
			PairedAt: time.Now().UTC().Format(time.RFC3339),
			// keep serving what the single pairing served before
			Policy: connector.Policy{Allow: []string{"*"}}}
		if missing {
			// The v1 key is gone: keep the pairing as a broken binding (state
			// error in status) the user can revoke and pair again, instead
			// of refusing to start.
			id.Bindings = append(id.Bindings, b)
			id.DeviceID, id.Version = "", storeVersion
			return writeJSON(f, id)
		}
		if v, err := openKey(raw, id.DataKey); err == nil {
			b.Issuer, b.Tenant, b.SaaSName = v.Issuer, v.Tenant, v.SaaSName
		}
		if err := os.MkdirAll(filepath.Join(dir, "bindings", bid), 0o700); err != nil {
			return err
		}
		if err := durable.Write(filepath.Join(dir, b.KeyFile), raw); err != nil {
			return err
		}
		if back, err := os.ReadFile(filepath.Join(dir, b.KeyFile)); err != nil || string(back) != string(raw) {
			return errors.New("migrate pairing: key copy verification failed")
		}
		id.Bindings = append(id.Bindings, b)
		id.DeviceID = ""
		id.Version = storeVersion
		if err := writeJSON(f, id); err != nil {
			if !durable.Committed(err) {
				// old identity.json intact: the new key copy is unreferenced
				_ = os.RemoveAll(filepath.Join(dir, "bindings", bid))
				return err
			}
			// Renamed, directory sync failed: the v2 file may reference the
			// new key, so never delete it. If it reads back as written the
			// migration is done; otherwise the next start decides (v1 again
			// → re-migrate, orphan cleanup drops this copy; v2 → uses it).
			want, _ := json.Marshal(id)
			if got, rerr := readFile(f); rerr != nil || !bytes.Equal(got, want) {
				return fmt.Errorf("migrate pairing: identity.json state uncertain, restart to retry: %w", err)
			}
		}
		// the .v1.bak copy stays. Review7 #16: if the original key cannot
		// be removed it is a plaintext-path leftover of the private key:
		// persist it for cleanup so every start retries (and revoke deletes it).
		if err := removeOne(old); err != nil && !errors.Is(err, os.ErrNotExist) {
			id.PendingCleanup = appendNew(id.PendingCleanup, deviceKeyFile)
			if werr := writeJSON(f, id); werr != nil {
				return fmt.Errorf("migrate pairing: record v1 key cleanup: %w", werr)
			}
		}
		return nil
	}
	id.Version = storeVersion
	return writeJSON(f, id)
}

func openKey(raw []byte, dataKey string) (*client.Identity, error) {
	c, err := durable.NewCipher(dataKey)
	if err != nil {
		return nil, err
	}
	var v client.Identity
	if err := c.Open(raw, &v); err != nil || !v.Valid() {
		return nil, errors.New("device key file is corrupt or encrypted with another key; pair again")
	}
	return &v, nil
}

func loadBinding(dir string, id localIdentity, b binding) (*client.Identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, b.KeyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("device key file is missing; revoke this connection and pair again")
	}
	if err != nil {
		return nil, err
	}
	return openKey(raw, id.DataKey)
}

func saveBinding(dir string, id localIdentity, b binding, v client.Identity) error {
	c, err := durable.NewCipher(id.DataKey)
	if err != nil {
		return err
	}
	raw, err := c.Seal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, b.KeyFile)), 0o700); err != nil {
		return err
	}
	return durable.Write(filepath.Join(dir, b.KeyFile), raw)
}

// removeBindingFiles deletes one binding's private keys (and, for a
// migrated binding, the v1 backup of the same key). It returns what could
// not be deleted; cleanupOrphans retries at the next start.
func removeBindingFiles(dir string, b binding) []string {
	var left []string
	if err := removeAll(filepath.Join(dir, "bindings", b.ID)); err != nil {
		left = append(left, err.Error())
	}
	if b.Migrated {
		for _, f := range []string{deviceKeyFile + ".v1.bak", deviceKeyFile} {
			if err := removeOne(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				left = append(left, err.Error())
			}
		}
	}
	return left
}

// bindingCleanupPaths are the private-key paths (relative to the data dir)
// to delete when a binding is revoked: its key dir and, for a migrated
// binding, the root v1 backup of the same key.
func bindingCleanupPaths(b binding) []string {
	p := []string{filepath.Join("bindings", b.ID)}
	if b.Migrated {
		p = append(p, deviceKeyFile+".v1.bak", deviceKeyFile)
	}
	return p
}

// runCleanup deletes paths; it returns those still present and why.
func runCleanup(dir string, paths []string) (left, errs []string) {
	for _, p := range paths {
		full := filepath.Join(dir, p)
		var err error
		if filepath.Dir(p) == "." { // root-level key files: never a tree
			err = removeOne(full)
		} else {
			err = removeAll(full)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			left, errs = append(left, p), append(errs, err.Error())
		}
	}
	return left, errs
}

// cleanupOrphans deletes key directories under bindings/ that identity.json
// does not reference (a revoke whose file deletion failed, or a pairing
// whose identity.json write failed), and returns what is still left.
func cleanupOrphans(dir string, id localIdentity) []string {
	ents, err := os.ReadDir(filepath.Join(dir, "bindings"))
	if err != nil {
		return nil
	}
	var left []string
	for _, e := range ents {
		if id.find(e.Name()) >= 0 {
			continue
		}
		if err := removeAll(filepath.Join(dir, "bindings", e.Name())); err != nil {
			left = append(left, err.Error())
		}
	}
	return left
}

func (id localIdentity) find(bid string) int {
	for i, b := range id.Bindings {
		if b.ID == bid {
			return i
		}
	}
	return -1
}

func policyAll() connector.Policy { return connector.Policy{Allow: []string{"*"}} }
