// lspmock runs the LMBP mock stack: gateway, SaaS, connector, fake model, or an all-in-one demo.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/mock/demo"
	"github.com/tyro668/LocalSubProxy/mock/fakemodel"
	"github.com/tyro668/LocalSubProxy/mock/saas"
	"github.com/tyro668/LocalSubProxy/mock/web"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lspmock demo|gateway|saas|connector|fakemodel [flags]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "demo":
		dir, _ := os.MkdirTemp("", "lspmock-demo-")
		defer os.RemoveAll(dir)
		_, err = demo.Run(ctx, os.Stdout, dir)
	case "fakemodel":
		f := flag.NewFlagSet("fakemodel", flag.ExitOnError)
		listen := f.String("listen", "127.0.0.1:11500", "")
		models := f.String("models", "qwen3-8b,broken", "")
		f.Parse(os.Args[2:])
		log.Printf("fake OpenAI-compatible model on http://%s", *listen)
		err = http.ListenAndServe(*listen, (&fakemodel.Server{Models: strings.Split(*models, ","), Delay: 30 * time.Millisecond}).Handler())
	case "gateway":
		f := flag.NewFlagSet("gateway", flag.ExitOnError)
		listen := f.String("listen", "127.0.0.1:8090", "")
		db := f.String("db", "gateway.db", "SQLite file")
		tenants := f.String("tenants", "acme:LSP_TENANT_KEY_ACME", "comma list of tenant:ENV_VAR_WITH_API_KEY")
		profiles := f.String("tenant-profiles", "", "optional JSON file with per-tenant SaaS display identity")
		retention := f.Duration("retention", 7*24*time.Hour, "delete finished tasks older than this (negative disables)")
		f.Parse(os.Args[2:])
		err = gateway.Serve(ctx, gateway.ServeOptions{Listen: *listen, DB: *db, Tenants: *tenants, Retention: *retention, AllowEphemeralKey: true, Profiles: *profiles, Wrap: web.Wrap})
	case "saas":
		f := flag.NewFlagSet("saas", flag.ExitOnError)
		listen := f.String("listen", "127.0.0.1:3000", "")
		public := f.String("public-url", "http://127.0.0.1:3000", "")
		gwURL := f.String("gateway", "http://127.0.0.1:8090", "gateway URL from this backend")
		pubGW := f.String("public-gateway", "", "gateway URL reachable from desktops (default: -gateway)")
		tenant := f.String("tenant", "acme", "")
		users := f.String("users", "alice:alice-pw,bob:bob-pw", "mock user:password list")
		f.Parse(os.Args[2:])
		u := map[string]string{}
		for _, p := range strings.Split(*users, ",") {
			a, b, _ := strings.Cut(p, ":")
			u[a] = b
		}
		key := os.Getenv("LSP_TENANT_KEY")
		if len(key) < 32 {
			log.Fatal("set LSP_TENANT_KEY (>=32 chars, same value as the gateway's tenant key)")
		}
		log.Printf("mock SaaS on %s", *public)
		err = serve(ctx, *listen, saas.New(saas.Config{PublicURL: *public, GatewayURL: *gwURL, PublicGatewayURL: *pubGW, TenantID: *tenant, TenantAPIKey: key, Users: u}).Handler())
	case "connector":
		f := flag.NewFlagSet("connector", flag.ExitOnError)
		saasURL := f.String("saas", "http://127.0.0.1:3000", "")
		id := f.String("identity", "lmbp-identity.json", "")
		name := f.String("name", hostname(), "")
		modelURL := f.String("model-url", "http://127.0.0.1:11500", "local OpenAI-compatible base URL")
		models := f.String("models", "qwen3-8b", "allowlisted model IDs")
		conc := f.Int("concurrency", 2, "")
		capsFlag := f.String("caps", "tools,vision", "model capabilities advertised for every allowlisted model (tools,vision,audio)")
		f.Parse(os.Args[2:])
		ident, ok, e := client.LoadIdentityFile(*id)
		if e != nil {
			log.Fatal(e)
		}
		if !ok {
			p, e := client.StartPairing(ctx, nil, *saasURL, *name)
			if e != nil {
				log.Fatal(e)
			}
			fmt.Printf("\n  配对码: %s\n  请在浏览器打开 %s 并确认\n\n", p.UserCode, p.LinkURL)
			if ident, e = p.Wait(ctx); e != nil {
				log.Fatal(e)
			}
			if e := client.SaveIdentityFile(*id, ident); e != nil {
				log.Fatal(e)
			}
			log.Printf("paired as %s", ident.DeviceID)
		}
		caps := map[string][]string{}
		for _, m := range strings.Split(*models, ",") {
			if *capsFlag != "" {
				caps[m] = strings.Split(*capsFlag, ",")
			}
		}
		c, e := client.New(client.Config{Executor: &client.OpenAIExecutor{BaseURL: *modelURL, ModelList: strings.Split(*models, ","), Caps: caps}, MaxConcurrency: *conc}, ident)
		if e != nil {
			log.Fatal(e)
		}
		err = c.Run(ctx, nil)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil && err != context.Canceled && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func serve(ctx context.Context, addr string, h http.Handler) error {
	s := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = s.Close() }()
	return s.ListenAndServe()
}

func hostname() string { h, _ := os.Hostname(); return h }
