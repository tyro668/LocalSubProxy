//go:build windows

package durable

import "os"

// platformSyncDir is a no-op on Windows: directory handles opened by os.Open
// cannot be flushed (FlushFileBuffers returns ERROR_ACCESS_DENIED), and NTFS
// journals the rename metadata itself. The file contents are still synced
// before os.Rename (MoveFileEx with MOVEFILE_REPLACE_EXISTING).
func platformSyncDir(*os.File) error { return nil }
