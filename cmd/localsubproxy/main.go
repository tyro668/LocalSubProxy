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
	"localsubproxy/internal/server"
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
		proto := f.String("protocol", "v2", "v2 (multi-tenant lsp2 gateway, default) or v1 (legacy single-device gateway)")
		listen := f.String("listen", "127.0.0.1:8080", "HTTP listen address; use TLS reverse proxy for remote access")
		db := f.String("db", "./data/gateway.db", "v2: SQLite database file")
		tenants := f.String("tenants", "default:LSP_TENANT_KEY_DEFAULT", "v2: comma list of tenant:ENV_VAR_WITH_API_KEY")
		retention := f.Duration("retention", 7*24*time.Hour, "v2: delete finished tasks older than this (negative disables)")
		data := f.String("data", "./data/server.json", "v1: durable server state file")
		device := f.String("device-id", "dev_local", "v1: configured device ID")
		requireBinding := f.Bool("require-binding", false, "v1: require owner binding before device connection and task submission")
		_ = f.Parse(os.Args[2:])
		if *proto == "v2" {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err = gateway.Serve(ctx, gateway.ServeOptions{Listen: *listen, DB: *db, Tenants: *tenants, Retention: *retention})
			break
		}
		if *proto != "v1" {
			err = fmt.Errorf("unknown protocol %q", *proto)
			break
		}
		log.Print("warning: running the legacy v1 gateway (single device, shared static tokens)")
		err = server.Serve(server.Config{DataFile: *data, RemoteToken: remoteServiceToken(), DeviceID: *device, DeviceToken: os.Getenv("LOCALSUBPROXY_DEVICE_TOKEN"), RequireBinding: *requireBinding}, *listen)
	case "pairing":
		f := flag.NewFlagSet("pairing", flag.ExitOnError)
		serverURL := f.String("server-url", "http://127.0.0.1:8080", "remote gateway base URL")
		device := f.String("device-id", "dev_local", "device ID")
		_ = f.Parse(os.Args[2:])
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var code connector.PairingCode
		code, err = connector.RequestPairing(ctx, *serverURL, *device, os.Getenv("LOCALSUBPROXY_DEVICE_TOKEN"))
		if err == nil {
			fmt.Printf("device: %s\ncode: %s\nexpires: %s\n", code.DeviceID, code.ShortCode, code.ExpiresAt.Format(time.RFC3339))
		}
	case "connector":
		f := flag.NewFlagSet("connector", flag.ExitOnError)
		configDir, _ := os.UserConfigDir()
		localDir := filepath.Join(configDir, "localsubproxy")
		proto := f.String("protocol", "v2", "v2 (default; pairs via the remote service) or v1 (legacy static device token)")
		remoteURL := f.String("remote-url", "", "v2: remote service origin that serves /.well-known/lsp-connector")
		identity := f.String("identity", filepath.Join(localDir, "lsp2-identity.json"), "v2: device identity file (0600; holds the device private key)")
		serverURL := f.String("server-url", "http://127.0.0.1:8080", "remote gateway base URL")
		device := f.String("device-id", "dev_local", "device ID")
		adapter := f.String("adapter-id", "", "optional initial adapter ID")
		adapterType := f.String("adapter-type", "ollama_http", "ollama_http or codex_app_server")
		ollama := f.String("ollama-url", "http://127.0.0.1:11434", "locally approved Ollama base URL")
		codexBin := f.String("codex-bin", "codex", "locally installed official Codex CLI executable")
		models := f.String("models", "", "comma separated allowed model IDs")
		journal := f.String("journal", filepath.Join(localDir, "journal.enc"), "encrypted local journal")
		adapterFile := f.String("adapter-file", filepath.Join(localDir, "adapters.enc"), "encrypted adapter configuration")
		journalMaxMiB := f.Int64("journal-max-mib", 64, "maximum encrypted journal size in MiB")
		_ = f.Parse(os.Args[2:])
		var xs []string
		for _, m := range strings.Split(*models, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				xs = append(xs, m)
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if *proto == "v2" {
			err = runV2Connector(ctx, *remoteURL, *identity, connector.Config{AdapterID: *adapter, AdapterType: *adapterType, OllamaURL: *ollama, CodexBin: *codexBin, AllowedModels: xs, JournalFile: *journal, JournalKey: os.Getenv("LOCALSUBPROXY_JOURNAL_KEY"), MaxJournalBytes: *journalMaxMiB << 20, AdapterFile: *adapterFile})
			if errors.Is(err, context.Canceled) {
				err = nil
			}
			break
		}
		c, e := connector.New(connector.Config{ServerURL: *serverURL, DeviceID: *device, DeviceToken: os.Getenv("LOCALSUBPROXY_DEVICE_TOKEN"), AdapterID: *adapter, AdapterType: *adapterType, OllamaURL: *ollama, CodexBin: *codexBin, AllowedModels: xs, JournalFile: *journal, JournalKey: os.Getenv("LOCALSUBPROXY_JOURNAL_KEY"), MaxJournalBytes: *journalMaxMiB << 20, AdapterFile: *adapterFile})
		if e != nil {
			err = e
			break
		}
		err = c.Run(ctx)
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

func usage() {
	fmt.Fprintln(os.Stderr, "usage: localsubproxy server [-protocol v2|v1] [flags] | connector [-protocol v2|v1] [flags] | pairing (v1 only) | codex-models [flags]")
}

func remoteServiceToken() string {
	if token := os.Getenv("LOCALSUBPROXY_REMOTE_TOKEN"); token != "" {
		return token
	}
	return os.Getenv("LOCALSUBPROXY_SAAS_TOKEN") // Existing deployments.
}

// runV2Connector pairs (first run) and then serves tasks over lsp2 using the same core as the desktop app.
func runV2Connector(ctx context.Context, remoteURL, identityFile string, cfg connector.Config) error {
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
	cfg.V2, cfg.RemoteURL = &id, id.RemoteURL
	c, err := connector.New(cfg)
	if err != nil {
		return err
	}
	return c.Run(ctx)
}
