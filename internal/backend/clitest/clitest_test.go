package clitest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A tree kill can land mid-write; the heartbeat file must never read back
// empty once the first beat exists, or the suite reports "never started".
func TestHeartbeatNeverEmpty(t *testing.T) {
	beat := filepath.Join(t.TempDir(), "beat")
	done := make(chan struct{})
	go func() { heartbeat(beat, 50); close(done) }()
	started := false
	for {
		select {
		case <-done:
			if !started {
				t.Fatal("no beat written")
			}
			return
		default:
		}
		b, err := os.ReadFile(beat)
		if err == nil && len(b) > 0 {
			started = true
		} else if started {
			t.Fatalf("heartbeat read back empty after starting (err=%v)", err)
		}
		time.Sleep(50 * time.Microsecond)
	}
}
