package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyro668/avora/internal/lmbp"
)

// audit3 P2-9: the provider's model list says which configured models
// exist; only those are published (and described by capabilities/names).
// Was: the probe filtered the list for the health view but every configured
// model was announced, so requests for a missing model reached the device.
// skip_model_probe keeps the configured list.
func TestPublishOnlyProbedModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "m1"}}})
	}))
	defer srv.Close()
	x, err := New(Config{MaxConcurrency: 1, DataKey: codexTestDataKey, AdapterFile: filepath.Join(t.TempDir(), "a.enc")})
	if err != nil {
		t.Fatal(err)
	}
	defer x.Shutdown()
	for _, a := range []AdapterConfig{
		{ID: "p", Name: "Probed", Type: "openai_compatible", BaseURL: srv.URL + "/v1", APIKey: "k", Models: []string{"m1", "m2"}, Enabled: true},
		{ID: "s", Name: "Skipped", Type: "openai_compatible", BaseURL: srv.URL + "/v1", APIKey: "k", Models: []string{"x1", "x2"}, Enabled: true, SkipModelProbe: true},
	} {
		if _, err := x.CreateAdapter(a); err != nil {
			t.Fatal(err)
		}
	}
	x.probeAll(context.Background())
	got := strings.Join(x.Models(), ",")
	if got != "p/m1,s/x1,s/x2" {
		t.Fatalf("published %q, want p/m1,s/x1,s/x2 (m2 is not offered by the provider)", got)
	}
	if _, ok := x.ModelCapabilities()["p/m2"]; ok {
		t.Fatal("model_capabilities describes the unpublished p/m2")
	}
	if _, ok := x.ModelNames()["p/m2"]; ok {
		t.Fatal("model_names describes the unpublished p/m2")
	}
}

func longModels(prefix string, n int) []string {
	ms := make([]string, n)
	for i := range ms {
		ms[i] = fmt.Sprintf("%s-%03d-%s", prefix, i, strings.Repeat("q", 240))
	}
	return ms
}

// audit3 P2-1: a model list within MaxModels can still exceed one frame
// (5 schemes x 100 IDs of 256 characters, listed in models, model_capabilities and
// model_names). Saving such a scheme is refused; schemes loaded from disk
// are published only as far as the encoded frame fits; disabled schemes
// are not described in the metadata. Was: saved, and the hello made the
// gateway close the connection with 1009 on every reconnect.
func TestAnnouncementFitsOneFrame(t *testing.T) {
	x, err := New(Config{MaxConcurrency: 1, DataKey: codexTestDataKey, AdapterFile: filepath.Join(t.TempDir(), "a.enc")})
	if err != nil {
		t.Fatal(err)
	}
	defer x.Shutdown()
	var saveErr error
	for i := 0; i < 5 && saveErr == nil; i++ { // 5 x 100 = MaxModels: allowed by count alone
		_, saveErr = x.CreateAdapter(AdapterConfig{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("C%d", i), Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1", APIKey: "k", Models: longModels(fmt.Sprintf("c%d", i), 100), Enabled: true, SkipModelProbe: true})
	}
	if saveErr == nil || !strings.Contains(saveErr.Error(), "bytes") {
		t.Fatalf("saving schemes whose announcement exceeds one frame: err %v", saveErr)
	}

	loaded := []AdapterConfig{{ID: "off", Name: "Off", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1", APIKey: "k", Models: []string{"z"}, SkipModelProbe: true}}
	for i := 0; i < 5; i++ {
		loaded = append(loaded, AdapterConfig{ID: fmt.Sprintf("a%d", i), Name: fmt.Sprintf("A%d", i), Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1", APIKey: "k", Models: longModels(fmt.Sprintf("a%d", i), 100), Enabled: true, SkipModelProbe: true})
	}
	y, err := New(Config{MaxConcurrency: 1, Adapters: loaded})
	if err != nil {
		t.Fatal(err)
	}
	defer y.Shutdown()
	b, _ := json.Marshal(lmbp.Envelope{V: lmbp.Version, Type: lmbp.TypeHello, ID: "h1", Data: func() []byte {
		d, _ := json.Marshal(lmbp.Hello{DeviceID: "dev_x", Client: "c", Models: y.Models(), ModelCapabilities: y.ModelCapabilities(), ModelNames: y.ModelNames(), MaxConcurrency: 1, Proof: strings.Repeat("p", 600)})
		return d
	}()})
	if len(b) > lmbp.MaxFrameBytes {
		t.Fatalf("published hello is %d bytes > max_frame_bytes %d", len(b), lmbp.MaxFrameBytes)
	}
	if len(y.Models()) == 0 {
		t.Fatal("nothing published: the first scheme fits on its own")
	}
	if _, ok := y.ModelCapabilities()["off/z"]; ok {
		t.Fatal("model_capabilities describes a disabled scheme's model")
	}
	pub := map[string]bool{}
	for _, m := range y.Models() {
		pub[m] = true
	}
	for m := range y.ModelCapabilities() {
		if !pub[m] {
			t.Fatalf("model_capabilities lists unpublished %s", m)
		}
	}
	for m := range y.ModelNames() {
		if !pub[m] {
			t.Fatalf("model_names lists unpublished %s", m)
		}
	}
}
