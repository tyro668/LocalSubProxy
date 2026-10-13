package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"localsubproxy/internal/connector"
	"localsubproxy/internal/lsp2/client"
	"localsubproxy/internal/lsp2/gateway"
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
		_ = f.Parse(os.Args[2:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = gateway.Serve(ctx, gateway.ServeOptions{Listen: *listen, DB: *db, Tenants: *tenants, Retention: *retention})
	case "connector":
		f := flag.NewFlagSet("connector", flag.ExitOnError)
		configDir, _ := os.UserConfigDir()
		localDir := filepath.Join(configDir, "localsubproxy")
		remoteURL := f.String("remote-url", "", "remote service origin that serves /.well-known/lsp-connector (first run)")
		identity := f.String("identity", filepath.Join(localDir, "lsp2-identity.json"), "device identity file (0600; holds the device private keys)")
		adapter := f.String("adapter-id", "", "optional initial adapter ID")
		adapterType := f.String("adapter-type", "ollama_http", "ollama_http or codex_app_server")
		ollama := f.String("ollama-url", "http://127.0.0.1:11434", "locally approved Ollama base URL")
		codexBin := f.String("codex-bin", "codex", "locally installed official Codex CLI executable")
		models := f.String("models", "", "comma separated allowed model IDs")
		caps := f.String("capabilities", "", "comma separated model capability flags for the initial adapter (tools,vision,audio)")
		conc := f.Int("max-concurrency", 4, "tasks served in parallel")
		adapterFile := f.String("adapter-file", filepath.Join(localDir, "adapters.enc"), "encrypted adapter configuration")
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
		err = runConnector(ctx, *remoteURL, *identity, cfg)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
	default:
		usage()
		os.Exit(2)
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
	fmt.Fprintln(os.Stderr, "usage: localsubproxy server [flags] | connector [flags] | codex-models [flags]")
}

// runConnector pairs (first run) and then serves tasks over lsp2 using the same core as the desktop app.
func runConnector(ctx context.Context, remoteURL, identityFile string, cfg connector.Config) error {
	id, ok, err := client.LoadIdentityFile(identityFile)
	if err != nil {
		return err
	}
	if !ok {
		if remoteURL == "" {
			return errors.New("first run: -remote-url is required to pair this device")
		}
		p, err := client.StartPairing(ctx, nil, remoteURL, connector.DeviceName())
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
