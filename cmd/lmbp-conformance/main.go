// Command lmbp-conformance runs the lmbp black-box conformance suite against
// any gateway implementation (Java, Node.js, Python, Go, Rust, ...).
//
//	LMBP_TENANT_KEY=... lmbp-conformance -gateway https://gw.example -tenant acme -schemas spec/schemas
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp/conformance"
)

func main() {
	gw := flag.String("gateway", "http://127.0.0.1:8787", "gateway base URL")
	tenant := flag.String("tenant", "default", "tenant id")
	user := flag.String("end-user", "conformance-user", "X-End-User for tenant calls")
	schemas := flag.String("schemas", "spec/schemas", "JSON Schema directory (empty to skip frame validation)")
	timeout := flag.Duration("timeout", 10*time.Second, "per-step timeout")
	issuer := flag.String("issuer", "", "expected gateway issuer origin (default: origin of -gateway)")
	asJSON := flag.Bool("json", false, "print results as JSON")
	flag.Parse()
	key := os.Getenv("LMBP_TENANT_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "LMBP_TENANT_KEY must hold the tenant API key")
		os.Exit(2)
	}
	cfg := conformance.Config{GatewayURL: *gw, Issuer: *issuer, Tenant: *tenant, TenantKey: key, EndUser: *user, SchemaDir: *schemas, Timeout: *timeout}
	if !*asJSON {
		cfg.Log = os.Stdout
	}
	res := conformance.Run(context.Background(), cfg)
	fail := 0
	for _, r := range res {
		if !r.Pass {
			fail++
		}
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"results": res, "failed": fail})
	} else {
		fmt.Printf("\n%d checks, %d failed\n", len(res), fail)
	}
	if fail > 0 {
		os.Exit(1)
	}
}
