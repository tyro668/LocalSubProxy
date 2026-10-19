package jsonrpc

import (
	"strings"
	"testing"
)

func TestFrames(t *testing.T) {
	b, err := Request("2.0", 7, "session/new", map[string]any{"cwd": "/tmp/x"})
	if err != nil || string(b) != `{"jsonrpc":"2.0","id":7,"method":"session/new","params":{"cwd":"/tmp/x"}}`+"\n" {
		t.Fatalf("%s %v", b, err)
	}
	b, _ = Request("", 1, "initialize", map[string]any{})
	if strings.Contains(string(b), "jsonrpc") {
		t.Fatalf("Codex dialect must omit the version: %s", b)
	}
	for line, kind := range map[string]string{
		`{"id":3,"result":{}}`:                         "response",
		`{"id":"a","method":"fs/read_text_file"}`:      "request",
		`{"method":"session/update","params":{}}`:      "notification",
		`{"jsonrpc":"2.0","id":4,"error":{"code":-1}}`: "response",
	} {
		m, err := Parse([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		got := map[bool]string{true: "request"}[m.IsRequest()] + map[bool]string{true: "response"}[m.IsResponse()] + map[bool]string{true: "notification"}[m.IsNotification()]
		if got != kind {
			t.Fatalf("%s: %s", line, got)
		}
	}
	if m, _ := Parse([]byte(`{"id":3,"result":{}}`)); func() bool { id, ok := m.IntID(); return !ok || id != 3 }() {
		t.Fatal("numeric id")
	}
	for _, bad := range []string{`not json`, `{}`, `{"result":{}}`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	r, _ := Refuse("2.0", []byte(`"a"`), "unsupported")
	if string(r) != `{"jsonrpc":"2.0","id":"a","error":{"code":-32601,"message":"unsupported"}}`+"\n" {
		t.Fatalf("%s", r)
	}
	if _, err := Request("", 1, "x", strings.Repeat("a", MaxMessage)); err == nil {
		t.Fatal("oversized message accepted")
	}
}
