package backend

import (
	"strings"
	"testing"

	"github.com/tyro668/avora/internal/lmbp"
)

// The estimate stays at or below real tokenizer counts (cl100k/o200k: a
// repeated common word "token " is 1 token per 6 bytes, the densest normal
// English; "上下文" is 2-3 tokens), so it only refuses clear overruns.
func TestEstimateInputTokensIsLow(t *testing.T) {
	r := func(s string) lmbp.TaskRun { return lmbp.TaskRun{Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text(s)}}} }
	for _, c := range []struct {
		text string
		max  int // a real tokenizer count (upper bound for the estimate)
	}{{strings.Repeat("token ", 1000), 1001}, {strings.Repeat("上下文", 100), 300}, {"", 1}} {
		if got := EstimateInputTokens(r(c.text)); got > c.max+1 {
			t.Fatalf("%q…: estimate %d above real count %d", c.text[:min(len(c.text), 12)], got, c.max)
		}
	}
	if CheckContextBudget(0, "", r(strings.Repeat("x", 1e6)), Config{}) != nil {
		t.Fatal("no window must mean no check")
	}
}
