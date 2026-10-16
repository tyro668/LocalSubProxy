package external

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Strict mode (CI / LMBP_REQUIRE_EXTERNAL) never skips the external test:
// without LMBP_EXTERNAL_GATEWAY it runs hermetically against the in-process
// reference gateway; with LMBP_EXPECT_EXTERNAL=1 (mock-interop) a missing
// gateway URL is a failure, never a silent fallback.
func TestExternalGatewayStrictModeNeverSkips(t *testing.T) {
	if os.Getenv("LMBP_CI_MODE_CHILD") != "" {
		t.Skip("child process")
	}
	run := func(extra ...string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestExternalGateway$", "-test.v")
		env := append([]string{"LMBP_CI_MODE_CHILD=1"}, extra...)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "LMBP_EXTERNAL_GATEWAY=") && !strings.HasPrefix(kv, "CI=") && !strings.HasPrefix(kv, "LMBP_REQUIRE_EXTERNAL=") && !strings.HasPrefix(kv, "LMBP_EXPECT_EXTERNAL=") && !strings.HasPrefix(kv, "LMBP_OFFICIAL_CLIENTS=") {
				env = append(env, kv)
			}
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, ci := range []string{"CI=true", "LMBP_REQUIRE_EXTERNAL=1"} {
		out, err := run(ci)
		if err != nil || !strings.Contains(out, "--- PASS: TestExternalGateway") || strings.Contains(out, "--- SKIP") || !strings.Contains(out, "in-process reference gateway") {
			t.Fatalf("%s: hermetic run did not pass (err %v):\n%s", ci, err, out)
		}
	}
	out, err := run("CI=true", "LMBP_EXPECT_EXTERNAL=1")
	if err == nil || !strings.Contains(out, "LMBP_EXPECT_EXTERNAL") || strings.Contains(out, "--- SKIP: TestExternalGateway") {
		t.Fatalf("expect-external without URL did not fail (err %v):\n%s", err, out)
	}
}
