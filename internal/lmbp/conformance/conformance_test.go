package conformance

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

func runAgainst(t *testing.T, cfg gateway.Config, verbose bool) []Result {
	t.Helper()
	st, err := store.OpenSQLite(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// A short retention so E6 (retention purges task + idempotency record) runs too.
	const retention = 2 * time.Second
	cfg.Store, cfg.Logger, cfg.PingInterval, cfg.Retention = st, log.New(io.Discard, "", 0), 200*time.Millisecond, retention
	g, err := gateway.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("k", 40)
	if err := g.RegisterTenant(context.Background(), "acme", "Acme", key); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go g.RunRetention(ctx, 0)
	s := httptest.NewServer(g.Handler())
	t.Cleanup(s.Close)
	var logw io.Writer
	if verbose && testing.Verbose() {
		logw = os.Stdout
	}
	return Run(context.Background(), Config{GatewayURL: s.URL, Tenant: "acme", TenantKey: key, SchemaDir: "../../../spec/schemas", Timeout: 5 * time.Second, Log: logw, Retention: retention})
}

// The Go reference gateway must pass its own conformance suite.
func TestReferenceGatewayConforms(t *testing.T) {
	res := runAgainst(t, gateway.Config{}, true)
	if len(res) < 25 {
		t.Fatalf("only %d checks ran", len(res))
	}
	ran := map[string]bool{}
	for _, r := range res {
		ran[r.ID] = true
		if r.Req == "" {
			t.Errorf("%s is not mapped to a requirement in catalog.go", r.ID)
		}
		if r.Skip {
			t.Errorf("%s %s: skipped (%s)", r.ID, r.Title, r.Detail)
		} else if !r.Pass {
			t.Errorf("%s %s: %s", r.ID, r.Title, r.Detail)
		}
	}
	for _, rq := range Catalog { // every catalogued check exists and ran
		for _, c := range rq.Checks {
			if !ran[c] {
				t.Errorf("%s (%s) is in the catalog but did not run", c, rq.ID)
			}
		}
	}
}

// A Core-only gateway (profiles core + openai, capabilities session.token
// and models.update: no resume, flow, tools, multimodal, e2ee, anthropic)
// passes every Core check; extension checks are skipped, never failed.
func TestCoreOnlyGatewayConforms(t *testing.T) {
	// no log: its SKIP lines would trip verify-all's skip scanner (they are
	// asserted below: only undeclared profiles may be skipped)
	res := runAgainst(t, gateway.Config{CoreOnly: true}, false)
	byID := map[string][]Result{}
	for _, r := range res {
		byID[r.ID] = append(byID[r.ID], r)
		if !r.Pass {
			t.Errorf("%s %s: %s", r.ID, r.Title, r.Detail)
		}
	}
	for _, rq := range Catalog {
		declared := rq.Profile == lmbp.ProfileCore || rq.Profile == lmbp.ProfileOpenAI
		for _, c := range rq.Checks {
			rs := byID[c]
			if len(rs) == 0 {
				t.Errorf("%s (%s) has no result", c, rq.ID)
				continue
			}
			for _, r := range rs {
				if declared && r.Skip {
					t.Errorf("%s (%s, declared profile %s) was skipped: %s", c, rq.ID, rq.Profile, r.Detail)
				}
				if !declared && !r.Skip {
					t.Errorf("%s (%s, undeclared profile %s) ran instead of being skipped", c, rq.ID, rq.Profile)
				}
			}
		}
	}
}
