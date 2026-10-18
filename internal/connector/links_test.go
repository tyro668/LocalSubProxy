package connector

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tyro668/avora/internal/audit"
	"github.com/tyro668/avora/internal/lmbp"
	"github.com/tyro668/avora/internal/lmbp/client"
	"github.com/tyro668/avora/mock/fakemodel"
)

func testLink(id, issuer, tenant string, p *Policy) *link {
	return &link{cfg: LinkConfig{ID: id, Policy: p, Identity: client.Identity{Issuer: issuer, Tenant: tenant, DeviceID: "dev_same", SaaSName: tenant}}, done: closedChan()}
}

func newPool(t *testing.T, maxc int, links ...*link) (*Connector, *audit.Log, string) {
	t.Helper()
	model := httptest.NewServer((&fakemodel.Server{Models: []string{"m1", "m2"}, Delay: 30 * time.Millisecond}).Handler())
	t.Cleanup(model.Close)
	al, err := audit.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { al.Close() })
	x, err := New(Config{MaxConcurrency: maxc, Audit: al, Adapters: []AdapterConfig{{ID: "lm", Type: "openai_compatible", BaseURL: model.URL, APIKey: "k", Models: []string{"m1", "m2"}, Enabled: true, SkipModelProbe: true, Concurrency: 8}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		x.links[l.cfg.ID] = l
		x.order = append(x.order, l.cfg.ID)
	}
	return x, al, model.URL
}

func task(id, model string) lmbp.TaskRun {
	return lmbp.TaskRun{TaskID: id, Model: model, Messages: []lmbp.Message{{Role: "user", Content: lmbp.Text("hello")}}}
}

// Two gateways may pick the same task id; the tasks must not collide.
func TestSameTaskIDFromTwoGateways(t *testing.T) {
	a, b := testLink("ba", "https://gw.a", "acme", &Policy{Allow: []string{"*"}}), testLink("bb", "https://gw.b", "acme", &Policy{Allow: []string{"*"}})
	x, _, _ := newPool(t, 4, a, b)
	var wg sync.WaitGroup
	fails := make([]*lmbp.ErrorBody, 2)
	for i, l := range []*link{a, b} {
		wg.Add(1)
		go func(i int, l *link) {
			defer wg.Done()
			_, fails[i] = x.serve(context.Background(), l, task("task_same", "lm/m1"), func(lmbp.TaskDelta) error { return nil })
		}(i, l)
	}
	time.Sleep(15 * time.Millisecond)
	x.mu.Lock()
	n := len(x.running)
	x.mu.Unlock()
	wg.Wait()
	if fails[0] != nil || fails[1] != nil {
		t.Fatalf("collision: %v %v", fails[0], fails[1])
	}
	if n != 2 {
		t.Fatalf("running %d, want both tasks tracked separately", n)
	}
	// same id on the same connection is still a duplicate
	x.mu.Lock()
	x.running["ba\x00task_dup"] = runSlot{adapter: "lm", link: "ba"}
	x.mu.Unlock()
	if _, f := x.serve(context.Background(), a, task("task_dup", "lm/m1"), func(lmbp.TaskDelta) error { return nil }); f == nil || f.Code != lmbp.ErrBadRequest {
		t.Fatalf("duplicate on one connection: %v", f)
	}
	// audit attributes each task to its own gateway
	recs, _ := x.AuditList(context.Background(), audit.Filter{Gateway: "https://gw.b"})
	if len(recs) != 1 || recs[0].Gateway != "https://gw.b" {
		t.Fatalf("audit %+v", recs)
	}
	st, _ := x.UsageStats(context.Background(), audit.Filter{}, []string{audit.BySaaS}, false)
	if len(st) != 2 {
		t.Fatalf("same tenant id on two gateways must be two SaaS buckets: %+v", st)
	}
}

func TestAllowListEnforced(t *testing.T) {
	a := testLink("ba", "https://gw.a", "acme", &Policy{Allow: []string{"lm/m1"}})
	d := testLink("bd", "https://gw.d", "deny", &Policy{Allow: []string{}})
	w := testLink("bw", "https://gw.w", "wild", &Policy{Allow: []string{"lm/*"}})
	x, _, _ := newPool(t, 4, a, d, w)
	if m := (linkExec{x, a}).Models(); len(m) != 1 || m[0] != "lm/m1" {
		t.Fatalf("a models %v", m)
	}
	if m := (linkExec{x, d}).Models(); len(m) != 0 {
		t.Fatalf("default deny leaked %v", m)
	}
	if m := (linkExec{x, w}).Models(); len(m) != 2 {
		t.Fatalf("wildcard %v", m)
	}
	if c := (linkExec{x, a}).ModelCapabilities(); len(c) != 1 {
		t.Fatalf("caps leaked %v", c)
	}
	noop := func(lmbp.TaskDelta) error { return nil }
	if _, f := (linkExec{x, a}).Execute(context.Background(), task("t1", "lm/m2"), noop); f == nil || f.Code != lmbp.ErrForbidden {
		t.Fatalf("m2 must be forbidden for a: %v", f)
	}
	if _, f := (linkExec{x, d}).Execute(context.Background(), task("t2", "lm/m1"), noop); f == nil || f.Code != lmbp.ErrForbidden {
		t.Fatalf("deny-all: %v", f)
	}
	if _, f := (linkExec{x, a}).Execute(context.Background(), task("t3", "lm/m1"), noop); f != nil {
		t.Fatalf("allowed model failed: %v", f)
	}
}

func TestFairShareAndCaps(t *testing.T) {
	a := testLink("ba", "https://gw.a", "acme", &Policy{Allow: []string{"*"}})
	b := testLink("bb", "https://gw.b", "beta", &Policy{Allow: []string{"*"}, MaxConcurrency: 1})
	x, _, _ := newPool(t, 2, a, b)
	ad := x.adapters["lm"]
	x.mu.Lock()
	defer x.mu.Unlock()
	if f := x.admitLocked(a, ad); f != nil {
		t.Fatal(f)
	}
	x.running["ba\x00t1"] = runSlot{"lm", "ba"}
	if f := x.admitLocked(a, ad); f == nil {
		t.Fatal("a exceeded its fair share while b is owed a slot")
	}
	if f := x.admitLocked(b, ad); f != nil {
		t.Fatalf("b must get its share: %v", f)
	}
	x.running["bb\x00t1"] = runSlot{"lm", "bb"}
	if f := x.admitLocked(b, ad); f == nil {
		t.Fatal("global cap exceeded")
	}
	delete(x.running, "ba\x00t1")
	if f := x.admitLocked(b, ad); f == nil {
		t.Fatal("per-connection cap (1) exceeded")
	}
	// b paused: a may use the whole device
	delete(x.running, "bb\x00t1")
	b.cfg.Paused = true
	x.running["ba\x00t1"] = runSlot{"lm", "ba"}
	if f := x.admitLocked(a, ad); f != nil {
		t.Fatalf("idle device capacity must be usable: %v", f)
	}
}

func TestStatusConnectionsAndFlatPrimary(t *testing.T) {
	a := testLink("ba", "https://gw.a", "acme", &Policy{Allow: []string{"*"}})
	b := testLink("bb", "https://gw.b", "beta", &Policy{Allow: []string{}})
	x, _, _ := newPool(t, 4, a, b)
	a.remoteOK, a.gatewayOK, b.revoked = true, true, true
	s := x.Status()
	cs := s["connections"].([]map[string]any)
	if len(cs) != 2 || s["connections_online"] != 1 || s["tenant"] != "acme" || s["connected"] != true {
		t.Fatalf("status %v", s)
	}
	if cs[1]["state"] != "revoked" || cs[0]["state"] != "online" {
		t.Fatalf("states %v %v", cs[0]["state"], cs[1]["state"])
	}
	if err := x.RemoveLink("ba"); err != nil {
		t.Fatal(err)
	}
	if s := x.Status(); s["tenant"] != "beta" || s["connections_total"] != 1 {
		t.Fatalf("after remove %v", s)
	}
}
