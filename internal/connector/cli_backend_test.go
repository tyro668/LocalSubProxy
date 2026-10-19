package connector

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyro668/avora/internal/lmbp"
)

func TestCLIConfigurationRoundTrip(t *testing.T) {
	for _, typ := range []string{"grok_build", "antigravity_cli"} {
		t.Run(typ, func(t *testing.T) {
			cfg := Config{DataKey: strings.Repeat("01", 32), AdapterFile: filepath.Join(t.TempDir(), "adapters.enc")}
			x, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "missing-cli")
			a := AdapterConfig{ID: "cli", Type: typ, Name: "CLI", Executable: path, Models: []string{"m"}, Enabled: false}
			view, err := x.CreateAdapter(a)
			if err != nil {
				t.Fatal(err)
			}
			if view.Executable != path || view.Concurrency != 1 || strings.Join(view.Capabilities, ",") != lmbp.ModelCapParams {
				t.Fatalf("view %+v", view)
			}
			y, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := y.ListAdapters(); len(got) != 1 || got[0].Executable != path || got[0].Type != typ {
				t.Fatalf("reload %+v", got)
			}
			if _, err = y.PatchAdapter("cli", []byte(`{"name":"renamed"}`)); err != nil {
				t.Fatal(err)
			}
			if got := y.ListAdapters()[0]; got.Executable != path || got.Name != "renamed" {
				t.Fatalf("patch %+v", got)
			}
			a.Enabled = true
			if err := validateSavedAdapter(&a); err == nil {
				t.Fatal("enabled missing executable accepted")
			}
			if err := validateLoadedAdapter(&a); err != nil {
				t.Fatalf("missing runtime should not brick load: %v", err)
			}
			h := y.probeConfig(context.Background(), a)
			if h.Health != "unavailable" {
				t.Fatal(h)
			}
			for _, r := range []lmbp.TaskRun{{MaxTokens: 1}, {Tools: []lmbp.Tool{{Type: "function"}}}, {Messages: []lmbp.Message{{Role: "user", Content: lmbp.Content{Parts: []lmbp.ContentPart{{Type: "image_url", ImageURL: &lmbp.ImageURL{URL: "data:image/png;base64,eA=="}}}}}}}} {
				if fail := checkCaps(a, r); fail == nil || fail.Code != lmbp.ErrUnsupported {
					t.Fatalf("unsupported request accepted: %+v", r)
				}
			}
		})
	}
}

func TestReasoningConfigurationRoundTrip(t *testing.T) {
	allowed := map[string][]string{
		"codex_app_server":  {"off", "none", "minimal", "low", "medium", "high", "xhigh"},
		"grok_build":        {"none", "minimal", "low", "medium", "high", "xhigh", "max"},
		"antigravity_cli":   {"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
		"openai_compatible": {"off", "low", "medium", "high"},
	}
	for typ, efforts := range allowed {
		for _, effort := range []string{"", "default", "off", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "bogus"} {
			t.Run(typ+"/"+effort, func(t *testing.T) {
				cfg := Config{DataKey: strings.Repeat("01", 32), AdapterFile: filepath.Join(t.TempDir(), "adapters.enc")}
				x, err := New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				a := AdapterConfig{ID: "effort", Name: "effort", Type: typ, CodexBin: "/missing/codex", Executable: "/missing/cli", Models: []string{"m"}, ReasoningEffort: effort, BaseURL: "http://127.0.0.1:1234/v1", APIKey: "k", Enabled: false}
				ok := effort == "" || effort == "default"
				for _, e := range efforts {
					ok = ok || e == effort
				}
				_, err = x.CreateAdapter(a)
				if !ok {
					if err == nil || !strings.Contains(err.Error(), "not available") {
						t.Fatalf("%s accepted for %s: %v", effort, typ, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				y, err := New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				want := effort
				if effort == "default" {
					want = ""
				}
				if effort == "off" && typ == "codex_app_server" {
					want = "none"
				}
				if got := y.ListAdapters()[0].ReasoningEffort; got != want {
					t.Fatalf("effort %q, want %q", got, want)
				}
			})
		}
	}
}
