package lmbp

import (
	"os"
	"strings"
	"testing"
)

// audit2 #6: one version rule everywhere — the subprotocol is always `lmbp`,
// the major version lives only in the envelope v. No document may still ask
// for a new subprotocol name per major version or advertise LMBP 1.0, and
// the release notes say that gateway, SDK and desktop upgrade together.
func TestVersionRuleIsConsistent(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile("../../" + p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if Subprotocol != "lmbp" || Version != 2 {
		t.Fatalf("wire identifiers: subprotocol %q, v %d", Subprotocol, Version)
	}
	for _, p := range []string{"docs/lmbp-spec.md", "docs/lmbp-2-p2-draft.md", "docs/implementers-guide.md", "docs/implementations.md", "docs/release.md", "README.md", "docs/README.md"} {
		s := read(p)
		for _, bad := range []string{"新的子协议名", "递增主版本和子协议名", "同时提供多个子协议", "LMBP 1.0（正式版）", "**LMBP 1.0**", "LMBP 1.0 正式版", "lmbp.2"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s still says %q", p, bad)
			}
		}
	}
	spec := read("docs/lmbp-spec.md")
	if !strings.Contains(spec, "子协议名固定为 `lmbp`") || !strings.Contains(spec, "MUST 递增信封的 `v`") {
		t.Error("spec §11 must state the single rule: fixed subprotocol lmbp + envelope major version")
	}
	if !strings.Contains(read("README.md"), "LMBP 2.0") {
		t.Error("README must name LMBP 2.0")
	}
	if rel := read("docs/release.md"); !strings.Contains(rel, "## LMBP 2.0 升级") || !strings.Contains(rel, "必须一起升级") {
		t.Error("docs/release.md lacks the deploy note: gateway, SDK and desktop upgrade together (2.0 rejects 1.x)")
	}
}
