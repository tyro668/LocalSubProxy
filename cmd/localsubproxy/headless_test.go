package main

import (
	"strings"
	"testing"

	"github.com/tyro668/LocalSubProxy/internal/connector"
)

func TestConnectionLabelFollowsSession(t *testing.T) {
	cases := []struct {
		c    connView
		want string
	}{
		{connView{State: "connected", Connected: true}, "online"},
		{connView{State: "connected", Connected: false}, "offline"}, // never online without a live session
		{connView{State: "connecting"}, "connecting"},
		{connView{State: "error"}, "error"},
		{connView{State: "connected", Connected: true, Paused: true}, "paused"},
	}
	for _, c := range cases {
		if got := c.c.label(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.c, got, c.want)
		}
	}
}

func TestAPIKeyNeverOnArgv(t *testing.T) {
	for _, a := range [][]string{{"--api-key", "x"}, {"-api-key=x"}, {"--key=x"}} {
		if err := rejectKeyArgs(a); err == nil {
			t.Errorf("%v accepted", a)
		}
	}
	if err := rejectKeyArgs([]string{"--api-key-env", "K", "--api-key-stdin"}); err != nil {
		t.Error(err)
	}
	if err := schemeCmd([]string{"add", "--type", "openai", "--api-key", "sk"}); err == nil || !strings.Contains(err.Error(), "never accepted") {
		t.Fatal(err)
	}
}

func TestSchemeTypesAndNames(t *testing.T) {
	for short, full := range map[string]string{"ollama": "ollama_http", "openai": "openai_compatible", "anthropic": "anthropic_compatible", "codex": "codex_app_server", "codex_app_server": "codex_app_server"} {
		if got, err := schemeType(short); err != nil || got != full {
			t.Errorf("%s: %q %v", short, got, err)
		}
	}
	if _, err := schemeType("gemini"); err == nil {
		t.Error("unsupported type accepted")
	}
	if err := checkName(strings.Repeat("é", connector.MaxSchemeNameBytes/2)); err != nil {
		t.Error(err)
	}
	if err := checkName(strings.Repeat("é", connector.MaxSchemeNameBytes/2+1)); err == nil {
		t.Error("over-long name (bytes) accepted")
	}
	if got := nextID("ollama", []connector.AdapterView{{ID: "ollama"}, {ID: "ollama-2"}}); got != "ollama-3" {
		t.Error(got)
	}
	if err := schemeCmd([]string{"rename", "x"}); err == nil {
		t.Error("unknown subcommand accepted")
	}
}

func TestFlagsAfterArguments(t *testing.T) {
	f, c := newFlags("x")
	pos, err := parse(f, []string{"conn1", "--json", "m1,m2", "--data-dir", "/d"})
	if err != nil || strings.Join(pos, " ") != "conn1 m1,m2" || !*c.json || *c.dataDir != "/d" {
		t.Fatalf("%v %v %v %v", pos, err, *c.json, *c.dataDir)
	}
}

func TestClientsNeedServe(t *testing.T) {
	err := statusCmd([]string{"--data-dir", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatal(err)
	}
}
