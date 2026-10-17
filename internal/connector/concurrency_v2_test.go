package connector

import (
	"strings"
	"testing"
)

// LMBP 2.0 §7.4: max_concurrency is 1-16 on the wire and is rejected, never
// clamped. The local policy and connector config used to accept up to 64 and
// silently announce 16.
func TestPolicyMaxConcurrencyRejectedAboveProtocolCap(t *testing.T) {
	for _, n := range []int{17, 64} {
		err := validatePolicy(&Policy{MaxConcurrency: n})
		if err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("policy max_concurrency %d: want out-of-range error, got %v", n, err)
		}
	}
	for _, n := range []int{0, 1, 16} {
		if err := validatePolicy(&Policy{MaxConcurrency: n}); err != nil {
			t.Fatalf("policy max_concurrency %d rejected: %v", n, err)
		}
	}
	if _, err := New(Config{MaxConcurrency: 17}); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("connector max concurrency 17: want out-of-range error, got %v", err)
	}
}
