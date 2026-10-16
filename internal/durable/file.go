package durable

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteError tells how far a failed Write got. Committed is false when the
// old file is untouched (any failure before or in the rename) and true when
// the new file already replaced it but the directory sync failed, so the
// replacement is visible yet its durability is uncertain.
type WriteError struct {
	Phase     string // create, write, sync, close, rename, dirsync
	Committed bool
	Err       error
}

func (e *WriteError) Error() string { return e.Phase + " " + e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }

// Committed reports whether a Write error happened after the rename.
func Committed(err error) bool {
	var we *WriteError
	return errors.As(err, &we) && we.Committed
}

// Fault injection points (tests).
var (
	renameFile = os.Rename
	syncDir    = platformSyncDir
	syncFile   = func(f *os.File) error { return f.Sync() }
)

// Write replaces a snapshot only after the new file and its directory are synced.
func Write(path string, data []byte) error {
	fail := func(phase string, err error) error { return &WriteError{Phase: phase, Err: err} }
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fail("create", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*")
	if err != nil {
		return fail("create", err)
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return fail("create", err)
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return fail("write", err)
	}
	if err = syncFile(f); err != nil {
		f.Close()
		return fail("sync", err)
	}
	if err = f.Close(); err != nil {
		return fail("close", err)
	}
	if err = renameFile(f.Name(), path); err != nil {
		return fail("rename", err)
	}
	d, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = syncDir(d)
		d.Close()
	}
	if err != nil {
		return &WriteError{Phase: "dirsync", Committed: true, Err: err}
	}
	return nil
}

func WriteJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return &WriteError{Phase: "encode", Err: err}
	}
	return Write(path, b)
}

func ReadJSON(path string, value any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}

type Cipher struct{ aead cipher.AEAD }

func NewCipher(hexKey string) (*Cipher, error) {
	b, err := hex.DecodeString(hexKey)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("data key must be 32 bytes encoded as 64 hex characters")
	}
	block, err := aes.NewCipher(b)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: a}, nil
}

func (c *Cipher) Seal(value any) ([]byte, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, b, nil), nil
}

func (c *Cipher) Open(data []byte, value any) error {
	n := c.aead.NonceSize()
	if len(data) < n {
		return errors.New("encrypted file too short")
	}
	b, err := c.aead.Open(nil, data[:n], data[n:], nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}
