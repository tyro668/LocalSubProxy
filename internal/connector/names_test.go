package connector_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tyro668/LocalSubProxy/internal/connector"
)

func TestDefaultSchemeName(t *testing.T) {
	for _, c := range []struct {
		model string
		taken []string
		want  string
	}{
		{"gpt-6.1-sol", nil, "gpt-6.1-sol"},
		{"gpt-6.1-sol", []string{"GPT-6.1-SOL ", "deepseek-flash"}, "gpt-6.1-sol_2"},
		{"gpt-6.1-sol", []string{"gpt-6.1-sol", "gpt-6.1-sol_2"}, "gpt-6.1-sol_3"},
		{"m", []string{"m", "m_3"}, "m_2"},
	} {
		if got := connector.DefaultSchemeName(c.model, c.taken); got != c.want {
			t.Errorf("DefaultSchemeName(%q, %v) = %q, want %q", c.model, c.taken, got, c.want)
		}
	}
}

func TestSchemeNamesUnique(t *testing.T) {
	c := newAdapterCore(t, filepath.Join(t.TempDir(), "adapters.enc"))
	defer c.Shutdown()
	a := lmCfg // no name: kernel fills "<model>" / "<model>_<n>"
	if v, err := c.CreateAdapter(a); err != nil || v.Name != "m1" {
		t.Fatalf("create default name: %+v %v", v, err)
	}
	b := lmCfg
	b.ID, b.Models = "lm2", []string{"m1"}
	if v, err := c.CreateAdapter(b); err != nil || v.Name != "m1_2" {
		t.Fatalf("second default name: %+v %v", v, err)
	}
	d := lmCfg
	d.ID, d.Name = "lm3", "  M1 "
	if _, err := c.CreateAdapter(d); !errors.Is(err, connector.ErrDuplicateSchemeName) {
		t.Fatalf("duplicate create: %v", err)
	}
	// rename onto another scheme's name (patch and full update)
	if _, err := c.PatchAdapter("lm2", json.RawMessage(`{"name":"m1"}`)); !errors.Is(err, connector.ErrDuplicateSchemeName) {
		t.Fatalf("duplicate patch: %v", err)
	}
	full := lmCfg
	full.ID, full.Name = "lm2", "M1"
	if _, err := c.UpdateAdapter(full); !errors.Is(err, connector.ErrDuplicateSchemeName) {
		t.Fatalf("duplicate update: %v", err)
	}
	// keeping its own name (any case) is fine
	if _, err := c.PatchAdapter("lm2", json.RawMessage(`{"name":" M1_2 "}`)); err != nil {
		t.Fatal(err)
	}
	if got := storedOf(t, c, "lm2").Name; got != "M1_2" {
		t.Fatalf("trimmed name = %q", got)
	}
	// the advertised display names come from the real ListAdapters schemes
	names := c.ModelNames()
	for _, v := range c.ListAdapters() {
		if names[v.ID+"/"+v.Models[0]] != v.Name {
			t.Fatalf("model_names %v vs view %+v", names, v)
		}
	}
}

// Older configs may repeat names: they load, and are made unique in memory.
func TestLegacyDuplicateNamesLoad(t *testing.T) {
	a, b, e := lmCfg, lmCfg, lmCfg
	a.Name, b.ID, b.Name, e.ID, e.Name = "dup", "lm2", "Dup", "lm3", ""
	c, err := connector.New(connector.Config{MaxConcurrency: 1, Adapters: []connector.AdapterConfig{a, b, e}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()
	got := map[string]string{}
	for _, v := range c.ListAdapters() {
		got[v.ID] = v.Name
	}
	if got["lm"] != "dup" || got["lm2"] != "Dup (2)" || got["lm3"] != "m1" {
		t.Fatalf("names = %v", got)
	}
}
