package connector_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tyro668/LocalSubProxy/internal/connector"
)

func viewOf(t *testing.T, c *connector.Connector, id string) map[string]any {
	t.Helper()
	for _, v := range c.ListAdapters() {
		if v.ID == id {
			raw, _ := json.Marshal(v)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			return m
		}
	}
	t.Fatalf("adapter %s missing", id)
	return nil
}

func storedOf(t *testing.T, c *connector.Connector, id string) connector.AdapterView {
	for _, v := range c.ListAdapters() {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("adapter %s missing", id)
	return connector.AdapterView{}
}

// Review7 #7/#8: rename / set-default copy the real ListAdapters view, and the
// editor sends a partial body; neither may persist computed capabilities,
// drop configured concurrency/capabilities, or flip enabled.
func TestPatchAdapterPreservesConfigFromView(t *testing.T) {
	c := newAdapterCore(t, filepath.Join(t.TempDir(), "adapters.enc"))
	defer c.Shutdown()
	cfg := lmCfg
	cfg.Capabilities = []string{"tools"}
	cfg.Concurrency = 2
	cfg.Enabled = false
	if _, err := c.CreateAdapter(cfg); err != nil {
		t.Fatal(err)
	}
	check := func(step string) {
		t.Helper()
		v := storedOf(t, c, "lm")
		if !reflect.DeepEqual(v.ConfiguredCapabilities, []string{"tools"}) || v.ConfiguredConcurrency != 2 || v.Enabled || !v.HasAPIKey {
			t.Fatalf("%s: stored config changed: caps=%v conc=%d enabled=%v key=%v", step, v.ConfiguredCapabilities, v.ConfiguredConcurrency, v.Enabled, v.HasAPIKey)
		}
	}
	// rename: whole view copied (effective caps include computed markers)
	view := viewOf(t, c, "lm")
	if reflect.DeepEqual(view["capabilities"], []any{"tools"}) {
		t.Fatalf("precondition: view capabilities should be computed, got %v", view["capabilities"])
	}
	view["name"] = "renamed"
	body, _ := json.Marshal(view)
	if _, err := c.PatchAdapter("lm", body); err != nil {
		t.Fatal(err)
	}
	check("rename")
	if storedOf(t, c, "lm").Name != "renamed" {
		t.Fatal("rename lost")
	}
	// set default from view
	view = viewOf(t, c, "lm")
	view["is_default"] = true
	body, _ = json.Marshal(view)
	if _, err := c.PatchAdapter("lm", body); err != nil {
		t.Fatal(err)
	}
	check("set-default")
	// editor partial body: no capabilities/concurrency/enabled keys
	if _, err := c.PatchAdapter("lm", json.RawMessage(`{"id":"lm","models":["m2"],"base_url":"http://127.0.0.1:9"}`)); err != nil {
		t.Fatal(err)
	}
	check("editor")
	if v := storedOf(t, c, "lm"); !reflect.DeepEqual(v.Models, []string{"m2"}) || v.Name != "renamed" {
		t.Fatalf("partial edit: %+v", v)
	}
	// explicit change still applies
	if _, err := c.PatchAdapter("lm", json.RawMessage(`{"id":"lm","capabilities":["tools","vision"],"concurrency":3}`)); err != nil {
		t.Fatal(err)
	}
	if v := storedOf(t, c, "lm"); !reflect.DeepEqual(v.ConfiguredCapabilities, []string{"tools", "vision"}) || v.ConfiguredConcurrency != 3 {
		t.Fatalf("explicit edit lost: %+v", v)
	}
}
