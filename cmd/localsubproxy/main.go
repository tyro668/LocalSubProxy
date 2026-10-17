package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tyro668/LocalSubProxy/internal/audit"
	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/client"
	"github.com/tyro668/LocalSubProxy/internal/lmbp/gateway"
	"github.com/tyro668/LocalSubProxy/mock/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "codex-models":
		f := flag.NewFlagSet("codex-models", flag.ExitOnError)
		bin := f.String("codex-bin", "codex", "locally installed official Codex CLI executable")
		_ = f.Parse(os.Args[2:])
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var models []string
		models, err = connector.CodexModels(ctx, *bin)
		if err == nil {
			for _, model := range models {
				fmt.Println(model)
			}
		}
	case "server":
		f := flag.NewFlagSet("server", flag.ExitOnError)
		listen := f.String("listen", "127.0.0.1:8080", "HTTP listen address; use a TLS reverse proxy for remote access")
		db := f.String("db", "./data/gateway.db", "SQLite database file")
		tenants := f.String("tenants", "default:LSP_TENANT_KEY_DEFAULT", "comma list of tenant:ENV_VAR_WITH_API_KEY")
		retention := f.Duration("retention", 7*24*time.Hour, "delete finished tasks older than this (negative disables)")
		issuer := f.String("issuer", "", "public https origin of this gateway, e.g. https://gw.example.com (required)")
		profiles := f.String("tenant-profiles", "", "optional JSON file with per-tenant SaaS display identity (name, logo_uri, homepage_uri, policy_uri, requested_models, requested_scopes)")
		chatUI := f.Bool("chat-ui", false, "serve the mock SaaS chat page (mock/web) at / and /chat; development only, sign-in needs a tenant API key")
		_ = f.Parse(os.Args[2:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		o := gateway.ServeOptions{Listen: *listen, DB: *db, Tenants: *tenants, Retention: *retention, Issuer: *issuer, Profiles: *profiles}
		if *chatUI {
			o.Wrap = web.Wrap
		}
		err = gateway.Serve(ctx, o)
	case "connector":
		f := flag.NewFlagSet("connector", flag.ExitOnError)
		configDir, _ := os.UserConfigDir()
		localDir := filepath.Join(configDir, "localsubproxy")
		remoteURL := f.String("remote-url", "", "remote service origin that serves /.well-known/localsubproxy (first run)")
		pairLink := f.String("pair-link", "", "lmbp://pair?v=2&gateway=...&tenant=... deep link (first run, alternative to -remote-url)")
		yes := f.Bool("yes", false, "approve the pairing consent without prompting (automation only)")
		identity := f.String("identity", filepath.Join(localDir, "lmbp-identity.json"), "device identity file (0600; holds the device private keys)")
		adapter := f.String("adapter-id", "", "optional initial adapter ID")
		adapterType := f.String("adapter-type", "ollama_http", "ollama_http or codex_app_server")
		ollama := f.String("ollama-url", "http://127.0.0.1:11434", "locally approved Ollama base URL")
		codexBin := f.String("codex-bin", "codex", "locally installed official Codex CLI executable")
		models := f.String("models", "", "comma separated allowed model IDs")
		caps := f.String("capabilities", "", "comma separated model capability flags for the initial adapter (tools,vision,audio)")
		conc := f.Int("max-concurrency", 4, "tasks served in parallel")
		adapterFile := f.String("adapter-file", filepath.Join(localDir, "adapters.enc"), "encrypted adapter configuration")
		auditDB := f.String("audit-db", filepath.Join(localDir, "audit.db"), "local per-request audit log (empty disables)")
		auditKeep := f.Duration("audit-retention", audit.DefaultRetention, "delete audit rows older than this")
		_ = f.Parse(os.Args[2:])
		cfg := connector.Config{DataKey: os.Getenv("LOCALSUBPROXY_DATA_KEY"), AdapterFile: *adapterFile, MaxConcurrency: *conc}
		if *adapter != "" {
			a := connector.AdapterConfig{ID: *adapter, Type: *adapterType, CodexBin: *codexBin, Models: split(*models), Capabilities: split(*caps), Enabled: true}
			if a.Type == "ollama_http" {
				a.BaseURL = *ollama
			}
			cfg.Adapters = []connector.AdapterConfig{a}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if *auditDB != "" {
			_ = os.MkdirAll(filepath.Dir(*auditDB), 0o700)
			al, aerr := audit.Open(*auditDB)
			if aerr != nil {
				err = aerr
				break
			}
			defer al.Close()
			go al.RunRetention(ctx, *auditKeep, time.Hour)
			cfg.Audit = al
		}
		err = runConnector(ctx, *remoteURL, *pairLink, *yes, *identity, cfg)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
	case "usage", "audit":
		err = auditCmd(os.Args[1], os.Args[2:])
	case "-h", "-help", "--help", "help":
		usage()
		return
	default:
		cmd, ok := headlessCommands[os.Args[1]]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
			usage()
			os.Exit(2)
		}
		err = cmd(os.Args[2:])
		if errors.Is(err, flag.ErrHelp) {
			return
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func split(s string) []string {
	var xs []string
	for _, m := range strings.Split(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			xs = append(xs, m)
		}
	}
	return xs
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: localsubproxy <command> [flags]

Headless device (same data directory as the desktop app; --data-dir overrides):
  serve                              run the kernel in the foreground
  pair <url|lmbp://pair?...>         pair a SaaS [--allow m,..|*] [--yes] [--max-concurrency n]
  scheme add --type ollama|openai|anthropic|codex [--id] [--name] [--base-url] [--models]
             [--api-key-env VAR | --api-key-stdin] [--codex-bin] [--capabilities]
  scheme list | update <id> [flags] | rm <id> | test <id>
  status | connections
  revoke <connection-id>
  models set <connection-id> <scheme/model,...|*|none>
  usage | audit                      local usage statistics and audit log
  (all client commands accept --json and --data-dir)

Other:
  server [flags]                     LMBP gateway (SaaS side)
  codex-models [flags]               list models of the local Codex CLI
  connector [flags]                  legacy single-connection device (own identity file; use serve)
`)
}

// runConnector pairs (first run) and then serves tasks over lmbp using the same core as the desktop app.
func runConnector(ctx context.Context, remoteURL, pairLink string, yes bool, identityFile string, cfg connector.Config) error {
	id, ok, err := client.LoadIdentityFile(identityFile)
	if err != nil {
		return err
	}
	if !ok {
		var req *client.PairRequest
		switch {
		case pairLink != "":
			req, err = client.PrepareLink(ctx, nil, pairLink)
		case remoteURL != "":
			req, err = client.Prepare(ctx, nil, remoteURL)
		default:
			return errors.New("first run: -remote-url or -pair-link is required to pair this device")
		}
		if err != nil {
			return err
		}
		printConsent(req.Consent)
		if !yes && !confirm("Allow this service to use the models on this computer? [y/N] ") {
			return req.Deny()
		}
		p, err := req.Approve(ctx, connector.DeviceName())
		if err != nil {
			return err
		}
		fmt.Printf("\n  pairing code: %s\n  open %s while signed in and confirm\n\n", p.UserCode, p.LinkURL)
		if id, err = p.Wait(ctx); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(identityFile), 0o700); err != nil {
			return err
		}
		if err := client.SaveIdentityFile(identityFile, id); err != nil {
			return err
		}
		log.Printf("paired as %s", id.DeviceID)
	}
	cfg.Identity, cfg.RemoteURL = &id, id.RemoteURL
	c, err := connector.New(cfg)
	if err != nil {
		return err
	}
	return c.Run(ctx)
}

func printConsent(c client.Consent) {
	fmt.Printf("\n  %s wants to use models on this computer\n", c.SaaS.Name)
	fmt.Printf("    gateway:     %s (verified signature, key %s)\n    tenant:      %s\n", c.Gateway, c.GatewayKeyID, c.Tenant)
	for _, kv := range [][2]string{{"homepage", c.SaaS.HomepageURI}, {"privacy", c.SaaS.PolicyURI}, {"terms", c.SaaS.TosURI}, {"logo", c.SaaS.LogoURI}} {
		if kv[1] != "" {
			fmt.Printf("    %-12s %s\n", kv[0]+":", kv[1])
		}
	}
	fmt.Printf("    models:      %s\n    scopes:      %s\n\n", strings.Join(c.RequestedModels, ", "), strings.Join(c.RequestedScopes, ", "))
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	var ans string
	_, _ = fmt.Scanln(&ans)
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes"
}

// auditCmd prints the local audit log ("audit") or aggregated token usage ("usage").
func auditCmd(name string, args []string) error {
	configDir, _ := os.UserConfigDir()
	f := flag.NewFlagSet(name, flag.ExitOnError)
	db := f.String("audit-db", filepath.Join(configDir, "localsubproxy", "audit.db"), "audit database")
	since := f.Duration("since", 7*24*time.Hour, "only rows newer than this")
	saas := f.String("saas", "", "filter by SaaS (tenant) id")
	model := f.String("model", "", "filter by model")
	group := f.String("group", "saas,model,day", "usage: comma list of saas,model,day,hour,status,connection")
	local := f.Bool("local-time", false, "usage: bucket days/hours in local time instead of UTC")
	limit := f.Int("limit", 50, "audit: max rows")
	asJSON := f.Bool("json", false, "print JSON")
	purge := f.Duration("purge-older-than", 0, "audit: delete rows older than this and exit")
	_ = f.Parse(args)
	l, err := audit.Open(*db)
	if err != nil {
		return err
	}
	defer l.Close()
	ctx := context.Background()
	flt := audit.Filter{Tenant: *saas, Model: *model, Since: time.Now().Add(-*since), Limit: *limit}
	if name == "audit" && *purge > 0 {
		n, err := l.Purge(ctx, time.Now().Add(-*purge))
		fmt.Printf("removed %d rows\n", n)
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if name == "audit" {
		rows, err := l.List(ctx, flt)
		if err != nil || *asJSON {
			_ = enc.Encode(rows)
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "TIME\tSAAS\tMODEL\tSTATUS\tERROR\tPROMPT\tCOMPL\tLATENCY")
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%dms\n", r.At.Local().Format("2006-01-02 15:04:05"), r.Tenant, r.Model, r.Status, r.ErrorCode, r.PromptTokens, r.CompletionTokens, r.LatencyMS)
		}
		return w.Flush()
	}
	b, err := l.Stats(ctx, flt, split(*group), *local)
	if err != nil || *asJSON {
		_ = enc.Encode(b)
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SAAS\tMODEL\tDAY/HOUR\tREQUESTS\tERRORS\tPROMPT\tCOMPLETION\tTOTAL\tAVG LATENCY")
	for _, x := range b {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%.0fms\n", x.SaaS, x.Model, x.Day+x.Hour, x.Requests, x.Errors, x.PromptTokens, x.CompletionTokens, x.TotalTokens, x.AvgLatencyMS)
	}
	return w.Flush()
}
