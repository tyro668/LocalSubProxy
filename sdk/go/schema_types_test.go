package lmbpserver_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	lmbpserver "github.com/tyro668/LocalSubProxy/sdk/go"
)

// B1: the public request types are driven by the spec. Every property
// spec/openapi.yaml declares for ChatRequest / MessagesRequest must be a
// field of the Go type, so callers can send it without raw JSON.
func openapiProps(t *testing.T, schema string) []string {
	t.Helper()
	b, err := os.ReadFile("../../spec/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	var out []string
	in, props := false, false
	for _, l := range lines {
		ind := len(l) - len(strings.TrimLeft(l, " "))
		switch {
		case l == "    "+schema+":":
			in = true
			continue
		case !in:
			continue
		case strings.TrimSpace(l) != "" && ind <= 4:
			return out
		case l == "      properties:":
			props = true
			continue
		case props && strings.TrimSpace(l) != "" && ind <= 6:
			props = false
		case props && ind == 8 && strings.Contains(l, ":"):
			out = append(out, strings.SplitN(strings.TrimSpace(l), ":", 2)[0])
		}
	}
	if len(out) == 0 {
		t.Fatalf("no properties for %s", schema)
	}
	return out
}

func jsonFields(v any) map[string]bool {
	m := map[string]bool{}
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		m[strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	return m
}

func TestPublicRequestTypesCoverSchema(t *testing.T) {
	for _, c := range []struct {
		schema string
		v      any
	}{
		{"ChatRequest", lmbpserver.ChatRequest{}},
		{"MessagesRequest", lmbpserver.MessagesRequest{}},
	} {
		have := jsonFields(c.v)
		for _, p := range openapiProps(t, c.schema) {
			if !have[p] {
				t.Errorf("%s: spec property %q has no Go field", c.schema, p)
			}
		}
	}
}
