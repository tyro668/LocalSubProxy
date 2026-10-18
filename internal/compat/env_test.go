package compat

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestGetenvLegacyFallback(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	t.Setenv("LOCALSUBPROXY_TEST_KEY", "old")
	if got := Getenv("AVORA_TEST_KEY"); got != "old" {
		t.Fatalf("fallback = %q, want old", got)
	}
	if !strings.Contains(buf.String(), "LOCALSUBPROXY_TEST_KEY is deprecated") || !strings.Contains(buf.String(), "AVORA_TEST_KEY") {
		t.Fatalf("no deprecation warning: %q", buf.String())
	}
	buf.Reset()
	_ = Getenv("AVORA_TEST_KEY")
	if buf.Len() != 0 {
		t.Fatalf("warning repeated: %q", buf.String())
	}
	t.Setenv("AVORA_TEST_KEY", "new")
	if got := Getenv("AVORA_TEST_KEY"); got != "new" {
		t.Fatalf("new name must win, got %q", got)
	}
	if got := Getenv("OTHER_KEY"); got != "" {
		t.Fatalf("unprefixed key = %q", got)
	}
}
