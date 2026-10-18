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

	"github.com/tyro668/avora/internal/lmbp"
	"github.com/tyro668/avora/internal/lmbp/conformance"
)

func main() {
	gw := flag.String("gateway", "http://127.0.0.1:8787", "gateway base URL")
	tenant := flag.String("tenant", "default", "tenant id")
	user := flag.String("end-user", "conformance-user", "X-End-User for tenant calls")
	schemas := flag.String("schemas", "spec/schemas", "JSON Schema directory (empty to skip frame validation)")
	timeout := flag.Duration("timeout", 10*time.Second, "per-step timeout")
	issuer := flag.String("issuer", "", "expected gateway issuer origin (default: origin of -gateway)")
	asJSON := flag.Bool("json", false, "print results as JSON")
	list := flag.Bool("list", false, "print the requirement ↔ check table (spec §13.2) and exit")
	retention := flag.Duration("retention", 0, "the gateway's retention period; when set, check that finished tasks and idempotency records are purged after it (E6)")
	flag.Parse()
	if *list {
		fmt.Print(conformance.CatalogMarkdown())
		return
	}
	key := os.Getenv("LMBP_TENANT_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "LMBP_TENANT_KEY must hold the tenant API key")
		os.Exit(2)
	}
	cfg := conformance.Config{GatewayURL: *gw, Issuer: *issuer, Tenant: *tenant, TenantKey: key, EndUser: *user, SchemaDir: *schemas, Timeout: *timeout, Retention: *retention}
	if !*asJSON {
		cfg.Log = os.Stdout
	}
	res := conformance.Run(context.Background(), cfg)
	fail, skipped := 0, 0
	type tally struct{ pass, fail, skip int }
	byProfile := map[string]*tally{}
	for _, r := range res {
		t := byProfile[r.Profile]
		if t == nil {
			t = &tally{}
			byProfile[r.Profile] = t
		}
		switch {
		case !r.Pass:
			fail++
			t.fail++
		case r.Skip:
			skipped++
			t.skip++
		default:
			t.pass++
		}
	}
	if !*asJSON {
		fmt.Println()
		for _, p := range append(lmbp.Profiles, "") {
			if t := byProfile[p]; t != nil {
				if p == "" {
					p = "(unmapped)"
				}
				fmt.Printf("profile %-10s %3d passed, %d failed, %d skipped\n", p, t.pass, t.fail, t.skip)
			}
		}
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"results": res, "failed": fail, "skipped": skipped})
	} else if skipped > 0 {
		fmt.Printf("\n%d checks, %d failed, %d skipped\n", len(res), fail, skipped)
	} else {
		fmt.Printf("\n%d checks, %d failed\n", len(res), fail)
	}
	if fail > 0 {
		os.Exit(1)
	}
}
