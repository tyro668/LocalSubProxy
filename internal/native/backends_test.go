package native

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The test binary doubles as a signed-out Grok CLI.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "--no-auto-update" && slices.Contains(os.Args, "models") {
		fmt.Println("You are not authenticated. Run grok login.")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestBackendsAction(t *testing.T) {
	var response struct {
		OK   bool `json:"ok"`
		Data []struct {
			Type         string   `json:"type"`
			Short        string   `json:"short"`
			Label        string   `json:"label"`
			Executable   string   `json:"executable"`
			LoginCommand string   `json:"login_command"`
			LoginStatus  bool     `json:"login_status"`
			NeedsAPIKey  bool     `json:"needs_api_key"`
			Efforts      []string `json:"efforts"`
		} `json:"data"`
	}
	// Served without a running kernel: the desktop needs it to draw forms.
	if err := json.Unmarshal([]byte(Call(`{"action":"backends"}`)), &response); err != nil || !response.OK {
		t.Fatalf("%v %+v", err, response)
	}
	byType := map[string]int{}
	for i, b := range response.Data {
		byType[b.Type] = i
	}
	grok := response.Data[byType["grok_build"]]
	if len(response.Data) != 6 || grok.Short != "grok" || grok.Executable != "grok" || grok.LoginCommand != "grok login" || !grok.LoginStatus || grok.Efforts[0] != "default" || slices.Contains(grok.Efforts, "off") {
		t.Fatalf("%+v", response.Data)
	}
	if openai := response.Data[byType["openai_compatible"]]; !openai.NeedsAPIKey || openai.LoginStatus || openai.Executable != "" {
		t.Fatalf("%+v", openai)
	}
}

func TestLoginStatusAction(t *testing.T) {
	startKernel(t, t.TempDir())
	request, _ := json.Marshal(map[string]any{"action": "login_status", "adapter": map[string]any{"type": "grok_build", "executable": os.Args[0], "id": "g", "models": []string{"m"}, "enabled": true}})
	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Data  struct {
			State   string `json:"state"`
			Command string `json:"command"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(Call(string(request))), &response); err != nil || !response.OK || response.Data.State != "required" || response.Data.Command != "grok login" {
		t.Fatalf("%v %+v", err, response)
	}
}

// The desktop tests draw scheme forms from desktop/test/fixtures/backends.json;
// it must be exactly what this build answers. AVORA_UPDATE_FIXTURES=1 rewrites it.
func TestDesktopBackendsFixture(t *testing.T) {
	var response struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(Call(`{"action":"backends"}`)), &response); err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, response.Data, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	path := filepath.Join("..", "..", "desktop", "test", "fixtures", "backends.json")
	if os.Getenv("AVORA_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), pretty.Bytes()) {
		t.Fatalf("%s is stale; run AVORA_UPDATE_FIXTURES=1 go test ./internal/native -run TestDesktopBackendsFixture", path)
	}
}
