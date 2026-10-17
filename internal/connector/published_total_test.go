package connector

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

func hundred(id string) AdapterConfig {
	ms := make([]string, 100)
	for i := range ms {
		ms[i] = fmt.Sprintf("m%03d", i)
	}
	return AdapterConfig{ID: id, Type: "openai_compatible", BaseURL: "https://api.example/v1", APIKey: "sk-x", Models: ms, Enabled: true}
}

// Audit2 P2-1: the published list never exceeds hello's 500 models. Saving
// past it is refused; a saved list past it loads with the overflowing scheme
// isolated as a health error.
func TestPublishedModelTotalLimit(t *testing.T) {
	x, err := New(Config{DataKey: keyA, AdapterFile: filepath.Join(t.TempDir(), "a.enc")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := x.CreateAdapter(hundred(fmt.Sprintf("s%d", i))); err != nil {
			t.Fatalf("scheme %d: %v", i, err)
		}
	}
	if _, err := x.CreateAdapter(hundred("s5")); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("501st model saved: %v", err)
	}
	off := hundred("s5")
	off.Enabled = false
	if _, err := x.CreateAdapter(off); err != nil { // disabled schemes publish nothing
		t.Fatalf("disabled scheme: %v", err)
	}

	f := filepath.Join(t.TempDir(), "b.enc")
	var saved []AdapterConfig
	for i := 0; i < 6; i++ {
		saved = append(saved, hundred(fmt.Sprintf("s%d", i)))
	}
	sealAdapters(t, f, keyA, saved)
	y, err := New(Config{DataKey: keyA, AdapterFile: f})
	if err != nil {
		t.Fatalf("over-limit list failed the load: %v", err)
	}
	if n := len(y.Models()); n != lmbp.MaxModels {
		t.Fatalf("published %d models, want %d", n, lmbp.MaxModels)
	}
	if !strings.Contains(y.invalid["s5"], "500") {
		t.Fatalf("overflowing scheme not isolated: %v", y.invalid)
	}
	if _, ok := y.ModelNames()["s5/m000"]; ok {
		t.Fatal("isolated scheme still announced in model_names")
	}
}
