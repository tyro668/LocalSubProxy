package conformance

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The requirement table in spec §13.2 is exactly the catalog that
// lmbp-conformance -list prints, and the spec's check count matches it.
func TestSpecCatalogMatches(t *testing.T) {
	b, err := os.ReadFile("../../../docs/lmbp-spec.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i, j := strings.Index(s, "<!-- catalog:begin -->\n"), strings.Index(s, "<!-- catalog:end -->")
	if i < 0 || j < i {
		t.Fatal("spec §13.2 catalog markers not found")
	}
	if got, want := strings.TrimSpace(s[i+len("<!-- catalog:begin -->\n"):j]), strings.TrimSpace(CatalogMarkdown()); got != want {
		t.Fatalf("spec §13.2 differs from `lmbp-conformance -list`; paste its output between the catalog markers")
	}
	checks := map[string]bool{}
	for _, r := range Catalog {
		for _, c := range r.Checks {
			if checks[c] {
				t.Errorf("check %s is mapped to two requirements", c)
			}
			checks[c] = true
		}
	}
	m := regexp.MustCompile(`共 (\d+) 项检查`).FindStringSubmatch(s)
	if m == nil || m[1] != fmt.Sprint(len(checks)) {
		t.Fatalf("spec says %v checks, catalog has %d", m, len(checks))
	}
}
