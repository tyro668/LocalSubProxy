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
		listen := f.String("listen", "127.0.0.1:8080", "HTTP listen address; use TLS reverse proxy for remote access")
		data := f.String("data", "./data/server.json", "durable server state file")
		device := f.String("device-id", "dev_local", "configured device ID")
		requireBinding := f.Bool("require-binding", false, "require owner binding before device connection and task submission")
		_ = f.Parse(os.Args[2:])
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
		c, e := connector.New(connector.Config{ServerURL: *serverURL, DeviceID: *device, DeviceToken: os.Getenv("LOCALSUBPROXY_DEVICE_TOKEN"), AdapterID: *adapter, AdapterType: *adapterType, OllamaURL: *ollama, CodexBin: *codexBin, AllowedModels: xs, JournalFile: *journal, JournalKey: os.Getenv("LOCALSUBPROXY_JOURNAL_KEY"), MaxJournalBytes: *journalMaxMiB << 20, AdapterFile: *adapterFile})
		if e != nil {
			err = e
			break
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
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
	fmt.Fprintln(os.Stderr, "usage: localsubproxy server [flags] | pairing [flags] | connector [flags] | codex-models [flags]")
}

func remoteServiceToken() string {
	if token := os.Getenv("LOCALSUBPROXY_REMOTE_TOKEN"); token != "" {
		return token
	}
	return os.Getenv("LOCALSUBPROXY_SAAS_TOKEN") // Existing deployments.
}
