package native

// Compatibility for the product rename (LocalSubProxy → Avora, 2.1.x).
// Remove together with the other legacy-name shims after one release.
//
// The kernel's default data directory moved from <UserConfigDir>/localsubproxy
// to <UserConfigDir>/avora. On the first start that uses the default
// directory, the old directory is copied over (identity.json, bindings and
// their device keys, adapters.enc, audit.db, ...) so every connection keeps
// working without pairing again, and the old directory is then renamed to
// localsubproxy.backup-<time> as a backup.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

const (
	// DataDirName is the directory under os.UserConfigDir() the kernel uses by default.
	DataDirName = "avora"
	// legacyDataDirName is the pre-rename default (LocalSubProxy ≤ 2.0.x).
	legacyDataDirName = "localsubproxy"
	// MigrationFile records a completed migration inside the new directory.
	MigrationFile = "migrated-from-legacy.json"
)

// ErrLegacyDirInUse means the pre-rename app (or its headless serve) still
// runs against the old data directory, so it cannot be migrated yet.
var ErrLegacyDirInUse = fmt.Errorf("%w: the old LocalSubProxy app (or \"localsubproxy serve\") is still running; quit it and start Avora again so its connections can be moved over", ErrDataDirInUse)

// DefaultDataDir is <UserConfigDir>/avora.
func DefaultDataDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, DataDirName), nil
}

// Migration describes what migrateLegacy did (zero value: nothing).
type Migration struct {
	From   string `json:"from"`
	Backup string `json:"backup,omitempty"`
	At     string `json:"at"`
	// Warning is set when the copy succeeded but the old directory could
	// not be renamed to the backup name.
	Warning string `json:"warning,omitempty"`
}

func sameDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if goruntime.GOOS == "windows" || goruntime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// MigrateDefaultDataDir migrates the legacy directory when dir is the
// default data directory; any other directory (an explicit --data-dir or
// data_dir) is left alone.
func MigrateDefaultDataDir(dir string) (Migration, error) {
	def, err := DefaultDataDir()
	if err != nil || !sameDir(dir, def) {
		return Migration{}, nil
	}
	return migrateLegacy(dir, filepath.Join(filepath.Dir(def), legacyDataDirName))
}

func hasEntries(dir string) (bool, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(ents) > 0, nil
}

// migrateLegacy copies oldDir to newDir when newDir is missing or empty and
// oldDir has content. The copy is staged in newDir+".migrating" and renamed
// into place, so an interrupted migration is simply redone at the next start.
func migrateLegacy(newDir, oldDir string) (Migration, error) {
	if ok, err := hasEntries(newDir); err != nil || ok {
		return Migration{}, err
	}
	if ok, err := hasEntries(oldDir); err != nil || !ok {
		return Migration{}, err
	}
	m, err := copyLegacy(newDir, oldDir)
	if err != nil {
		return Migration{}, err
	}
	// Keep the old directory as a backup under a name the old app no longer
	// opens, so it cannot come back online with the same device keys. The
	// lock is released first: Windows cannot rename a directory with an
	// open file in it.
	backup := oldDir + ".backup-" + time.Now().Format("20060102-150405")
	if err := os.Rename(oldDir, backup); err != nil {
		m.Warning = fmt.Sprintf("connections were moved to %s, but the old directory %s could not be renamed to %s (%v); do not start the old LocalSubProxy app again", newDir, oldDir, backup, err)
	} else {
		m.Backup = backup
	}
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(newDir, MigrationFile), append(b, '\n'), 0o600)
	}
	log.Printf("avora: data directory migrated from %s to %s (backup: %s)", oldDir, newDir, m.Backup)
	return m, nil
}

// copyLegacy stages a copy of oldDir in newDir+".migrating" while holding
// the old kernel.lock (the old kernel holds it while it runs, so a live
// directory is never copied) and renames the stage to newDir. An interrupted
// migration leaves only the stage behind and is redone at the next start.
func copyLegacy(newDir, oldDir string) (Migration, error) {
	f, err := os.OpenFile(filepath.Join(oldDir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return Migration{}, fmt.Errorf("migrate %s: %w", oldDir, err)
	}
	defer f.Close()
	if err := tryLock(f); err != nil {
		if errors.Is(err, errWouldBlock) {
			return Migration{}, ErrLegacyDirInUse
		}
		return Migration{}, fmt.Errorf("migrate %s: %w", oldDir, err)
	}
	defer func() { _ = unlock(f) }()
	stage := newDir + ".migrating"
	fail := func(err error) (Migration, error) {
		_ = os.RemoveAll(stage)
		return Migration{}, fmt.Errorf("migrate %s to %s: %w", oldDir, newDir, err)
	}
	if err := os.RemoveAll(stage); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o700); err != nil {
		return fail(err)
	}
	if err := copyTree(oldDir, stage); err != nil {
		return fail(err)
	}
	if err := os.Remove(newDir); err != nil && !errors.Is(err, fs.ErrNotExist) { // an empty leftover
		return fail(err)
	}
	if err := os.Rename(stage, newDir); err != nil {
		return fail(err)
	}
	return Migration{From: oldDir, At: time.Now().UTC().Format(time.RFC3339)}, nil
}

// copyTree copies regular files and directories (keeping their permission
// bits); the lock file, sockets and other special files are skipped.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case rel == lockFile || !info.Mode().IsRegular():
			return nil
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
