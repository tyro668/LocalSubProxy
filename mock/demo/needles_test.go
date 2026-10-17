package demo

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyro668/LocalSubProxy/mock/fakemodel"
)

// The plaintext check must only fire on the secret: none of its needles may
// appear in the backpressure reply that shares the gateway store.
func TestE2EENeedlesAreSpecific(t *testing.T) {
	for _, n := range e2eeNeedles {
		if !strings.Contains(e2eeSecret, string(n)) {
			t.Fatalf("needle %q is not part of the secret", n)
		}
		for i := 0; i < fakemodel.VeryLongChunks; i++ {
			if bytes.Contains([]byte(fakemodel.VeryLongChunk(i)), n) {
				t.Fatalf("needle %q occurs in backpressure chunk %d", n, i)
			}
		}
	}
}
