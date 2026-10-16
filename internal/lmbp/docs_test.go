package lmbp

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// Review7 #18: the implementers guide uses the same frame version and
// conformance check count as the spec.
func TestImplementersGuideMatchesSpec(t *testing.T) {
	guide, err := os.ReadFile("../../docs/implementers-guide.md")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := os.ReadFile("../../docs/lmbp-spec.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`\{\s*"?v"?\s*:\s*(\d+)\s*,\s*"?type"?`).FindAllSubmatch(guide, -1) {
		if n, _ := strconv.Atoi(string(m[1])); n != Version {
			t.Fatalf("guide frame example uses v:%d, protocol is v%d: %s", n, Version, m[0])
		}
	}
	count := regexp.MustCompile(`共 (\d+) 项`)
	want := count.FindSubmatch(spec)
	if want == nil {
		t.Fatal("spec check count not found")
	}
	for _, m := range count.FindAllSubmatch(guide, -1) {
		if string(m[1]) != string(want[1]) {
			t.Fatalf("guide says %s checks, spec says %s", m[1], want[1])
		}
	}
	if got := regexp.MustCompile(`直到 (\d+) 项全部通过`).FindSubmatch(guide); got != nil && string(got[1]) != string(want[1]) {
		t.Fatalf("guide pass count %s != %s", got[1], want[1])
	}
}
