package durable

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Review2 #4: every phase reports whether the replacement is committed.
func TestWritePhases(t *testing.T) {
	boom := errors.New("injected")
	cases := []struct {
		name      string
		inject    func()
		committed bool
		want      string // file content afterwards
	}{
		{"file sync", func() { syncFile = func(*os.File) error { return boom } }, false, "old"},
		{"rename", func() { renameFile = func(string, string) error { return boom } }, false, "old"},
		{"dir sync", func() { syncDir = func(*os.File) error { return boom } }, true, "new"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "x.json")
			_ = os.WriteFile(f, []byte("old"), 0o600)
			c.inject()
			err := Write(f, []byte("new"))
			syncFile, renameFile, syncDir = func(f *os.File) error { return f.Sync() }, os.Rename, func(d *os.File) error { return d.Sync() }
			if err == nil || !errors.Is(err, boom) || Committed(err) != c.committed {
				t.Fatalf("err %v committed %v", err, Committed(err))
			}
			if b, _ := os.ReadFile(f); string(b) != c.want {
				t.Fatalf("content %q", b)
			}
			ents, _ := os.ReadDir(filepath.Dir(f))
			if len(ents) != 1 {
				t.Fatalf("temp file left: %v", ents)
			}
		})
	}
	if Committed(nil) || Committed(boom) {
		t.Fatal("Committed on non-write error")
	}
}
