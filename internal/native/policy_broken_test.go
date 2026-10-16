package native

import (
	"context"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/lmbp"
)

// Review6 P1-B: a loaded, serving link that is marked broken (a state
// change failed earlier) must still get a tightened policy: the next task
// is rejected.
func TestPolicyAppliedToBrokenLoadedLink(t *testing.T) {
	setupBindings(t, bA, bB)
	if _, err := runtime.core.CreateAdapter(connector.AdapterConfig{ID: "lm", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", APIKey: "k", Models: []string{"m1"}, Enabled: true, SkipModelProbe: true}); err != nil {
		t.Fatal(err)
	}
	if runtime.broken == nil {
		runtime.broken = map[string]string{}
	}
	runtime.broken["b_a"] = "apply saved pause state: injected"
	if !runtime.core.HasLink("b_a") {
		t.Fatal("fixture: b_a should be loaded")
	}
	must(t, map[string]any{"action": "connection_policy_set", "connection_id": "b_a", "policy": map[string]any{"allow": []string{}}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run := lmbp.TaskRun{TaskID: "task_x", Model: "lm/m1", Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hi")}}}
	_, fail := runtime.core.ExecuteOn(ctx, "b_a", run, func(lmbp.TaskDelta) error { return nil })
	if fail == nil || fail.Code != lmbp.ErrForbidden {
		t.Fatalf("task on b_a after allow=[]: %+v", fail)
	}
}
