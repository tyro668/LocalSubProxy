package native

import (
	"encoding/json"
	"strings"
	"testing"
)

// Two kernels (desktop app + `avora serve`) must not run against one
// data directory: the second start is refused with a clear error, and the
// lock is released by stop so the directory can be reopened.
func TestDataDirLockRefusesSecondKernel(t *testing.T) {
	dir := t.TempDir()
	other, err := lockDir(dir) // stands in for another process holding it
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(start(`{"data_dir":`+jsonString(dir)+`}`)), &r)
	if r.OK || !strings.Contains(r.Error, "data directory in use") {
		t.Fatalf("second kernel started on a locked data dir: %+v", r)
	}
	other.release()

	_ = json.Unmarshal([]byte(start(`{"data_dir":`+jsonString(dir)+`}`)), &r)
	if !r.OK {
		t.Fatalf("start after release: %s", r.Error)
	}
	if _, err := lockDir(dir); err == nil {
		t.Fatal("running kernel does not hold the data dir lock")
	}
	call(`{"action":"stop"}`)
	l, err := lockDir(dir)
	if err != nil {
		t.Fatalf("stop did not release the lock: %v", err)
	}
	l.release()
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
