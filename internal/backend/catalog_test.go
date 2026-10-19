package backend

import (
	"strings"
	"testing"
)

func TestResolveEffortRequiresEvidence(t *testing.T) {
	models := []ModelInfo{
		{ID: "unknown"},
		{ID: "fixed", ReasoningKnown: true},
		{ID: "switchable", ReasoningKnown: true, ReasoningEfforts: []EffortOption{{Value: "low", Model: "switchable-low"}, {Value: "none"}}},
	}
	for _, tc := range []struct{ model, effort, want string }{
		{"unknown", "", "unknown"}, {"missing", "", "missing"}, {"fixed", "", "fixed"},
		{"unknown", "low", ""}, {"fixed", "none", ""}, {"missing", "low", ""},
		{"switchable", "low", "switchable-low"}, {"switchable", "none", "switchable"}, {"switchable", "max", ""},
	} {
		got, err := ResolveEffort(models, tc.model, tc.effort)
		if tc.want == "" {
			if err == nil || !strings.Contains(err.Error(), "CAPABILITY_UNSUPPORTED") {
				t.Fatalf("%+v: %q %v", tc, got, err)
			}
		} else if err != nil || got != tc.want {
			t.Fatalf("%+v: %q %v", tc, got, err)
		}
	}
}
