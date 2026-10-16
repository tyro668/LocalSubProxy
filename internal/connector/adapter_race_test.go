package connector_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/connector"
)

func newAdapterCore(t *testing.T, file string) *connector.Connector {
	t.Helper()
	c, err := connector.New(connector.Config{DataKey: dataKey, AdapterFile: file, MaxConcurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var lmCfg = connector.AdapterConfig{ID: "lm", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "k", Models: []string{"m1"}, Enabled: true, SkipModelProbe: true}

// Review5 P2-a: a delete lands while an update is validating; the update
// must not write the deleted adapter back.
func TestUpdateDoesNotResurrectDeletedAdapter(t *testing.T) {
	file := filepath.Join(t.TempDir(), "adapters.enc")
	c := newAdapterCore(t, file)
	if _, err := c.CreateAdapter(lmCfg); err != nil {
		t.Fatal(err)
	}
	defer connector.SetMutHook(func() {
		if err := c.DeleteAdapter("lm"); err != nil {
			t.Error(err)
		}
	})()
	upd := lmCfg
	upd.Models = []string{"m2"}
	if _, err := c.UpdateAdapter(upd); err == nil {
		t.Fatal("update of a concurrently deleted adapter succeeded")
	}
	if len(c.Models()) != 0 {
		t.Fatalf("memory resurrected: %v", c.Models())
	}
	c.Shutdown()
	if m := newAdapterCore(t, file).Models(); len(m) != 0 {
		t.Fatalf("disk resurrected: %v", m)
	}
}

// Review5 P2-a: an update on the old core crossing stop/start must not
// overwrite what the new core saved: Shutdown waits for it, and changes
// on a stopped core are refused.
func TestOldCoreSaveCannotCrossRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "adapters.enc")
	old := newAdapterCore(t, file)
	if _, err := old.CreateAdapter(lmCfg); err != nil {
		t.Fatal(err)
	}
	inUpdate, release := make(chan struct{}), make(chan struct{})
	restore := connector.SetMutHook(func() { close(inUpdate); <-release })
	upd := lmCfg
	upd.Models = []string{"old"}
	go func() { _, _ = old.UpdateAdapter(upd) }()
	<-inUpdate
	down := make(chan struct{})
	go func() { old.Shutdown(); close(down) }()
	select {
	case <-down:
		t.Fatal("Shutdown returned while an adapter update was in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	<-down
	restore()
	if _, err := old.UpdateAdapter(upd); err == nil {
		t.Fatal("stopped core accepted an adapter change")
	}
	nw := newAdapterCore(t, file) // new core after the restart
	upd.Models = []string{"new"}
	if _, err := nw.UpdateAdapter(upd); err != nil {
		t.Fatal(err)
	}
	if _, err := old.UpdateAdapter(lmCfg); err == nil {
		t.Fatal("old core wrote after the new core started")
	}
	if m := newAdapterCore(t, file).Models(); len(m) != 1 || m[0] != "lm/new" {
		t.Fatalf("disk: %v", m)
	}
}
