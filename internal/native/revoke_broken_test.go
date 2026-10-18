package native

import (
	"errors"
	"slices"
	"testing"

	"github.com/tyro668/avora/internal/connector"
)

// Review5 P1: a pause is written but its read-back fails (uncertain), so
// the running link is not paused. Revoke reconciles first; applying the
// saved pause fails (a task is running) and the binding is marked broken.
// Revoke must still tear the running link down before saying removed.
func TestRevokeTearsDownLinkWhosePauseFailed(t *testing.T) {
	setupBindings(t, bA, bB)
	restore := injectPhase(t, uncertain)
	if r := do(t, map[string]any{"action": "connection_disconnect", "connection_id": "b_a"}); r.OK {
		t.Fatal("fixture: uncertain pause should fail")
	}
	restore()
	if !runtime.core.HasLink("b_a") || runtime.core.Closing("b_a") {
		t.Fatal("fixture: b_a should still be serving")
	}
	op := setPaused
	setPaused = func(*connector.Connector, string, bool) error {
		return errors.New("tasks are still running on this connection (injected)")
	}
	defer func() { setPaused = op }()
	r := do(t, map[string]any{"action": "connection_revoke", "connection_id": "b_a"})
	if r.OK && slices.Contains(runtime.core.ConnectionIDs(), "b_a") {
		t.Fatal("revoke reported removed while b_a is still loaded and serving")
	}
	if !r.OK && !runtime.core.HasLink("b_a") {
		t.Fatal("revoke failed but link vanished")
	}
	if !r.OK {
		t.Fatalf("revoke should succeed (no task actually running): %+v", r)
	}
}
