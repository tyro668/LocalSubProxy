package external

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// In CI (CI or LMBP_REQUIRE_EXTERNAL set) a missing gateway configuration is
// a failure, not a silent skip: a matrix that skipped the external checks
// must not report green.
func TestExternalGatewayFailsInCIWithoutEnv(t *testing.T) {
	if os.Getenv("LMBP_CI_MODE_CHILD") != "" {
		t.Skip("child process")
	}
	for _, ci := range []string{"CI=true", "LMBP_REQUIRE_EXTERNAL=1"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestExternalGateway$", "-test.v")
		env := []string{"LMBP_CI_MODE_CHILD=1", ci}
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "LMBP_EXTERNAL_GATEWAY=") && !strings.HasPrefix(kv, "CI=") && !strings.HasPrefix(kv, "LMBP_REQUIRE_EXTERNAL=") {
				env = append(env, kv)
			}
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "LMBP_EXTERNAL_GATEWAY") || strings.Contains(string(out), "--- SKIP: TestExternalGateway") {
			t.Fatalf("%s: external test did not fail without LMBP_EXTERNAL_GATEWAY (err %v):\n%s", ci, err, out)
		}
	}
}
