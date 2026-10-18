package connector

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/tyro668/avora/internal/durable"
)

// Review4 E: adapters.enc commit phases (rename ok, directory sync fails).
func TestAdapterFileCommitPhases(t *testing.T) {
	newC := func(t *testing.T) (*Connector, string) {
		f := filepath.Join(t.TempDir(), "adapters.enc")
		c, err := New(Config{DataKey: strings.Repeat("ab", 32), AdapterFile: f})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CreateAdapter(AdapterConfig{ID: "a", Type: "ollama_http", BaseURL: "http://127.0.0.1:1", Models: []string{"m"}, Enabled: true, SkipModelProbe: true}); err != nil {
			t.Fatal(err)
		}
		return c, f
	}
	syncFails := func(t *testing.T) func() {
		ow := writeAdapterFile
		n := 0
		writeAdapterFile = func(p string, b []byte) error {
			n++
			if err := ow(p, b); err != nil {
				return err
			}
			if n == 1 { // only the change itself; a restore write succeeds
				return &durable.WriteError{Phase: "dirsync", Committed: true, Err: errors.New("EIO (injected)")}
			}
			return nil
		}
		return func() { writeAdapterFile = ow }
	}
	ids := func(c *Connector) (out []string) {
		for _, v := range c.ListAdapters() {
			out = append(out, v.ID)
		}
		return
	}
	reopen := func(t *testing.T, f string) []string {
		c, err := New(Config{DataKey: strings.Repeat("ab", 32), AdapterFile: f})
		if err != nil {
			t.Fatal(err)
		}
		return ids(c)
	}

	t.Run("verified read-back counts as committed", func(t *testing.T) {
		c, f := newC(t)
		defer syncFails(t)()
		if _, err := c.CreateAdapter(AdapterConfig{ID: "b", Type: "ollama_http", BaseURL: "http://127.0.0.1:1", Models: []string{"m"}, Enabled: true, SkipModelProbe: true}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if len(ids(c)) != 2 || len(reopen(t, f)) != 2 || c.AdapterStorageWarning() == "" {
			t.Fatalf("memory %v disk %v warn %q", ids(c), reopen(t, f), c.AdapterStorageWarning())
		}
	})
	t.Run("unverifiable: previous content restored, memory rolled back", func(t *testing.T) {
		c, f := newC(t)
		before, _ := os.ReadFile(f)
		restore := syncFails(t)
		or := readAdapterFile
		readAdapterFile = func(string) ([]byte, error) { return nil, syscall.EIO }
		_, err := c.CreateAdapter(AdapterConfig{ID: "b", Type: "ollama_http", BaseURL: "http://127.0.0.1:1", Models: []string{"m"}, Enabled: true, SkipModelProbe: true})
		readAdapterFile = or
		restore()
		after, _ := os.ReadFile(f)
		if err == nil || len(ids(c)) != 1 || !bytes.Equal(before, after) || len(reopen(t, f)) != 1 {
			t.Fatalf("err %v memory %v disk-same %v", err, ids(c), bytes.Equal(before, after))
		}
	})
	t.Run("unverifiable and restore fails: further changes blocked", func(t *testing.T) {
		c, _ := newC(t)
		ow, or := writeAdapterFile, readAdapterFile
		writeAdapterFile = func(p string, b []byte) error {
			return &durable.WriteError{Phase: "dirsync", Committed: true, Err: errors.New("EIO (injected)")}
		}
		readAdapterFile = func(string) ([]byte, error) { return nil, syscall.EIO }
		_, err := c.CreateAdapter(AdapterConfig{ID: "b", Type: "ollama_http", BaseURL: "http://127.0.0.1:1", Models: []string{"m"}, Enabled: true, SkipModelProbe: true})
		writeAdapterFile, readAdapterFile = ow, or
		if err == nil || !strings.Contains(err.Error(), "uncertain") {
			t.Fatalf("err %v", err)
		}
		if err := c.DeleteAdapter("a"); err == nil || !strings.Contains(err.Error(), "restart") {
			t.Fatalf("change not blocked: %v", err)
		}
	})
}
