package connector

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// A saved scheme that no longer validates (here: a plain-HTTP URL to a
// non-loopback host, written by an older version or edited by hand) must
// not make the whole adapter list fail to load: it stays listed with a
// health error, is never advertised or served, and can be fixed or deleted.
func TestBrokenSavedSchemeLoadsAsHealthError(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "adapters.enc")
	good := oneAdapter[0]
	bad := AdapterConfig{ID: "ol", Type: "ollama_http", Name: "old ollama", BaseURL: "http://192.0.2.10:11434", Models: []string{"qwen3"}, Enabled: true}
	sealAdapters(t, f, keyA, []AdapterConfig{good, bad})

	x, err := New(Config{DataKey: keyA, AdapterFile: f})
	if err != nil {
		t.Fatalf("one broken scheme failed the whole load: %v", err)
	}
	var view AdapterView
	for _, v := range x.ListAdapters() {
		if v.ID == "ol" {
			view = v
		}
	}
	if view.ID == "" || view.Health != "unavailable" || !strings.Contains(view.HealthError, "invalid configuration") {
		t.Fatalf("broken scheme view %+v", view)
	}
	for _, m := range x.Models() {
		if strings.HasPrefix(m, "ol/") {
			t.Fatalf("broken scheme advertised: %v", x.Models())
		}
	}
	if h := x.probeAdapter(context.Background(), x.adapters["ol"]); h.Health != "unavailable" || h.Error == "" {
		t.Fatalf("broken scheme probed as %+v", h)
	}
	// still editable: a valid fix clears the error
	patch, _ := json.Marshal(map[string]any{"base_url": "http://127.0.0.1:11434"})
	if _, err := x.PatchAdapter("ol", patch); err != nil {
		t.Fatalf("fixing the broken scheme: %v", err)
	}
	for _, v := range x.ListAdapters() {
		if v.ID == "ol" && strings.Contains(v.HealthError, "invalid configuration") {
			t.Fatalf("fixed scheme still flagged: %+v", v)
		}
	}
	// and deletable
	sealAdapters(t, f, keyA, []AdapterConfig{good, bad})
	y, err := New(Config{DataKey: keyA, AdapterFile: f})
	if err != nil {
		t.Fatal(err)
	}
	if err := y.DeleteAdapter("ol"); err != nil {
		t.Fatalf("delete broken scheme: %v", err)
	}
	if len(y.ListAdapters()) != 1 {
		t.Fatalf("after delete: %+v", y.ListAdapters())
	}
}

// An entry that cannot be addressed (no ID) is skipped with a warning; the
// rest of the list still loads.
func TestUnaddressableSavedSchemeIsSkippedWithWarning(t *testing.T) {
	f := filepath.Join(t.TempDir(), "adapters.enc")
	sealAdapters(t, f, keyA, []AdapterConfig{oneAdapter[0], {Type: "ollama_http", Models: []string{"m"}}})
	x, err := New(Config{DataKey: keyA, AdapterFile: f})
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if len(x.ListAdapters()) != 1 || !strings.Contains(x.AdapterStorageWarning(), "unusable ID") {
		t.Fatalf("list %+v warning %q", x.ListAdapters(), x.AdapterStorageWarning())
	}
}
