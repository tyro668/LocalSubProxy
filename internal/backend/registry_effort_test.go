package backend

import (
	"strings"
	"testing"
)

func TestRegistryEffortsPerType(t *testing.T) {
	ok := func(*Config, bool) error { return nil }
	r := NewRegistry(
		Definition{Type: "cli", Label: "CLI", Short: "c", Aliases: []string{"cl"}, Efforts: []string{"low", "none"}, EffortAliases: map[string]string{"off": "none"}, Validate: ok, New: func(Config, Dependencies) Backend { return nil }},
		Definition{Type: "http", Label: "HTTP", Short: "h", Efforts: HTTPEfforts, Validate: ok, New: func(Config, Dependencies) Backend { return nil }},
	)
	for _, tc := range []struct{ typ, effort, stored, err string }{
		{"cli", "default", "", ""}, {"cli", "off", "none", ""}, {"cli", "low", "low", ""}, {"cli", "max", "", "not available"},
		{"http", "off", "off", ""}, {"http", "xhigh", "", "not available"},
	} {
		a := Config{Type: tc.typ, ReasoningEffort: tc.effort}
		err := r.Validate(&a, false)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%+v: %v", tc, err)
			}
			continue
		}
		if err != nil || a.ReasoningEffort != tc.stored {
			t.Errorf("%+v: %q %v", tc, a.ReasoningEffort, err)
		}
	}
	if d, ok := r.Find("cl"); !ok || d.Type != "cli" {
		t.Fatal("alias")
	}
	if _, ok := r.Find("x"); ok {
		t.Fatal("unknown found")
	}
}

func TestCheckEffortPassthrough(t *testing.T) {
	unknown := []ModelInfo{{ID: "m"}}
	known := []ModelInfo{{ID: "m", ReasoningKnown: true, ReasoningEfforts: []EffortOption{{Value: "low"}}}}
	strict := Definition{}
	pass := Definition{EffortPassthrough: true}
	if CheckEffort(strict, unknown, "m", "low") == nil {
		t.Error("strict accepted an unconfirmed effort")
	}
	if err := CheckEffort(pass, unknown, "m", "low"); err != nil {
		t.Error(err)
	}
	if CheckEffort(pass, known, "m", "high") == nil {
		t.Error("passthrough ignored a catalog that refuses the effort")
	}
	if err := CheckEffort(pass, known, "m", "low"); err != nil {
		t.Error(err)
	}
}
