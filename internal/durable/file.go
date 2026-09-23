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

// Write replaces a snapshot only after the new file and its directory are synced.
func Write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func WriteJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
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
		return nil, fmt.Errorf("journal key must be 32 bytes encoded as 64 hex characters")
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
		return errors.New("encrypted journal too short")
	}
	b, err := c.aead.Open(nil, data[:n], data[n:], nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}
