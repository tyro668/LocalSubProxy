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

	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/store"
)

// The Go reference gateway must pass its own conformance suite.
func TestReferenceGatewayConforms(t *testing.T) {
	st, err := store.OpenSQLite(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A short retention so E6 (retention purges task + idempotency record) runs too.
	const retention = 2 * time.Second
	g, err := gateway.New(gateway.Config{Store: st, Logger: log.New(io.Discard, "", 0), PingInterval: 200 * time.Millisecond, Retention: retention})
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("k", 40)
	if err := g.RegisterTenant(context.Background(), "acme", "Acme", key); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go g.RunRetention(ctx, 0)
	s := httptest.NewServer(g.Handler())
	defer s.Close()
	var logw io.Writer
	if testing.Verbose() {
		logw = os.Stdout
	}
	res := Run(context.Background(), Config{GatewayURL: s.URL, Tenant: "acme", TenantKey: key, SchemaDir: "../../../spec/schemas", Timeout: 5 * time.Second, Log: logw, Retention: retention})
	if len(res) < 25 {
		t.Fatalf("only %d checks ran", len(res))
	}
	for _, r := range res {
		if r.Skip {
			t.Errorf("%s %s: skipped (%s)", r.ID, r.Title, r.Detail)
		} else if !r.Pass {
			t.Errorf("%s %s: %s", r.ID, r.Title, r.Detail)
		}
	}
}
