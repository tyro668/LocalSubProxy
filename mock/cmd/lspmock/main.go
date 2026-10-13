// lspmock runs the protocol-v2 mock stack: gateway, SaaS, connector, fake model, or an all-in-one demo.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"localsubproxy/internal/lsp2"
	"localsubproxy/internal/lsp2/client"
	"localsubproxy/internal/lsp2/gateway"
	"localsubproxy/internal/lsp2/store"
	"localsubproxy/mock/demo"
	"localsubproxy/mock/fakemodel"
	"localsubproxy/mock/saas"
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
		retention := f.Duration("retention", 7*24*time.Hour, "delete finished tasks older than this (negative disables)")
		f.Parse(os.Args[2:])
		var c *store.Cipher
		if k := os.Getenv("LSP_DATA_KEY"); k != "" { // 64 hex chars → AES-256-GCM at rest
			kb, e := hex.DecodeString(k)
			if e != nil {
				log.Fatal("LSP_DATA_KEY must be hex")
			}
			if c, err = store.NewCipher(kb); err != nil {
				log.Fatal(err)
			}
		}
		var sk ed25519.PrivateKey
		if k := os.Getenv("LSP_SIGNING_SEED"); k != "" { // 64 hex chars; otherwise ephemeral
			kb, e := hex.DecodeString(k)
			if e != nil || len(kb) != ed25519.SeedSize {
				log.Fatal("LSP_SIGNING_SEED must be 32 bytes hex")
			}
			sk = ed25519.NewKeyFromSeed(kb)
		} else {
			log.Print("warning: LSP_SIGNING_SEED unset; using an ephemeral signing key (paired devices must re-pair after restart)")
		}
		st, e := store.OpenSQLite(*db, c)
		if e != nil {
			log.Fatal(e)
		}
		g, e := gateway.New(gateway.Config{Store: st, SigningKey: sk, Retention: *retention})
		if e != nil {
			log.Fatal(e)
		}
		for _, t := range strings.Split(*tenants, ",") {
			id, env, _ := strings.Cut(t, ":")
			if e := g.RegisterTenant(ctx, id, id, os.Getenv(env)); e != nil {
				log.Fatalf("tenant %s: %v (set %s)", id, e, env)
			}
		}
		go g.RunRetention(ctx, time.Hour)
		log.Printf("lsp2 gateway on http://%s (gateway key %s)", *listen, lsp2.B64(g.PublicKey()))
		err = serve(ctx, *listen, g.Handler())
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
		id := f.String("identity", "lsp2-identity.json", "")
		name := f.String("name", hostname(), "")
		modelURL := f.String("model-url", "http://127.0.0.1:11500", "local OpenAI-compatible base URL")
		models := f.String("models", "qwen3-8b", "allowlisted model IDs")
		conc := f.Int("concurrency", 2, "")
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
		c, e := client.New(client.Config{Executor: &client.OpenAIExecutor{BaseURL: *modelURL, ModelList: strings.Split(*models, ",")}, MaxConcurrency: *conc}, ident)
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
