package connector_test

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tyro668/avora/internal/connector"
)

// Review9 P2-3: scheme names are limited to 100 bytes, but the default name
// was cut to 90 characters (CJK: 270 bytes) and the de-duplication suffix
// was appended without re-checking. Such a name was saved, and the next
// start rejected the file: the whole kernel failed to start.
func TestLongCJKDefaultNameSurvivesRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "adapters.enc")
	c := newAdapterCore(t, file)
	model := strings.Repeat("模", 60) // 180 bytes: a valid model id (<= 200)
	a := lmCfg
	a.Models = []string{model}
	v, err := c.CreateAdapter(a)
	if err != nil {
		t.Fatal(err)
	}
	b := lmCfg
	b.ID, b.Models = "lm2", []string{model}
	w, err := c.CreateAdapter(b) // needs the "_2" suffix on top of a full-length base
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{v.Name, w.Name} {
		if len(n) > connector.MaxSchemeNameBytes || !utf8.ValidString(n) {
			t.Fatalf("default name %q is %d bytes (limit %d) or not valid UTF-8", n, len(n), connector.MaxSchemeNameBytes)
		}
	}
	if !strings.HasSuffix(w.Name, "_2") || v.Name == w.Name {
		t.Fatalf("names %q / %q", v.Name, w.Name)
	}
	c.Shutdown()
	c2, err := connector.New(connector.Config{DataKey: dataKey, AdapterFile: file, MaxConcurrency: 4})
	if err != nil {
		t.Fatalf("restart after saving default names failed: %v", err)
	}
	defer c2.Shutdown()
	if n := len(c2.ListAdapters()); n != 2 {
		t.Fatalf("schemes after restart: %d", n)
	}
}

// Legacy duplicates get " (n)": the suffixed name must still fit, so the next
// save (and start) keeps working.
func TestLegacyDuplicateLongNamesFit(t *testing.T) {
	long := strings.Repeat("名", 33) // 99 bytes: valid on its own
	a, b := lmCfg, lmCfg
	a.Name, b.ID, b.Name = long, "lm2", long
	file := filepath.Join(t.TempDir(), "adapters.enc")
	c, err := connector.New(connector.Config{DataKey: dataKey, AdapterFile: file, MaxConcurrency: 1, Adapters: []connector.AdapterConfig{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()
	seen := map[string]bool{}
	for _, v := range c.ListAdapters() {
		if len(v.Name) > connector.MaxSchemeNameBytes || !utf8.ValidString(v.Name) {
			t.Fatalf("de-duplicated name %q is %d bytes", v.Name, len(v.Name))
		}
		if seen[v.Name] {
			t.Fatalf("duplicate %q", v.Name)
		}
		seen[v.Name] = true
	}
}

func TestDefaultSchemeNameByteLimit(t *testing.T) {
	m := strings.Repeat("é", 80) // 160 bytes
	first := connector.DefaultSchemeName(m, nil)
	second := connector.DefaultSchemeName(m, []string{first})
	for _, n := range []string{first, second} {
		if len(n) > connector.MaxSchemeNameBytes || !utf8.ValidString(n) {
			t.Fatalf("%q: %d bytes", n, len(n))
		}
	}
	if !strings.HasSuffix(second, "_2") {
		t.Fatalf("second = %q", second)
	}
}

// A name over the limit written by an older version loads (cut to fit).
func TestOverLongLegacyNameLoads(t *testing.T) {
	a := lmCfg
	a.Name = strings.Repeat("长", 50) // 150 bytes
	c, err := connector.New(connector.Config{MaxConcurrency: 1, Adapters: []connector.AdapterConfig{a}})
	if err != nil {
		t.Fatalf("over-long legacy name bricked startup: %v", err)
	}
	defer c.Shutdown()
	if n := c.ListAdapters()[0].Name; len(n) > connector.MaxSchemeNameBytes || !utf8.ValidString(n) {
		t.Fatalf("name %q (%d bytes)", n, len(n))
	}
}
