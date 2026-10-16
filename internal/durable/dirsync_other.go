//go:build !windows

package durable

import "os"

// platformSyncDir fsyncs the directory so a completed rename survives a crash.
func platformSyncDir(d *os.File) error { return d.Sync() }
