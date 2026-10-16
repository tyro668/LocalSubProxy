package main

// Headless mode: `localsubproxy serve` runs the same kernel as the desktop
// app (internal/native, same data directory and files) in the foreground and
// exposes a local-only control channel (internal/control). The other
// subcommands here are clients of that channel.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/tyro668/LocalSubProxy/internal/connector"
	"github.com/tyro668/LocalSubProxy/internal/control"
	"github.com/tyro668/LocalSubProxy/internal/native"
)

// stdout/stdin are swapped in tests.
var (
	stdout io.Writer = os.Stdout
	stdin  io.Reader = os.Stdin
)

var headlessCommands = map[string]func([]string) error{
	"serve":       serveCmd,
	"pair":        pairCmd,
	"scheme":      schemeCmd,
	"status":      statusCmd,
	"connections": connectionsCmd,
	"revoke":      revokeCmd,
	"models":      modelsCmd,
}

func defaultDataDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return "localsubproxy"
	}
	return filepath.Join(d, "localsubproxy")
}

type common struct {
	dataDir *string
	json    *bool
}

func newFlags(name string) (*flag.FlagSet, common) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	return f, common{
		dataDir: f.String("data-dir", defaultDataDir(), "data directory (the desktop app's by default)"),
		json:    f.Bool("json", false, "print the kernel's JSON reply"),
	}
}

// parse accepts flags before and after positional arguments.
func parse(f *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := f.Parse(args); err != nil {
			return nil, err
		}
		args = f.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func call(c common, req any) (json.RawMessage, error) {
	cl, err := control.Connect(*c.dataDir)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	return do(cl, req)
}

// do sends one request and unwraps the {"ok","data"|"error"} reply.
func do(cl *control.Client, req any) (json.RawMessage, error) {
	raw, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	return envelope(string(raw))
}

func printJSON(v json.RawMessage) error {
	var x any
	if err := json.Unmarshal(v, &x); err != nil {
		_, err = fmt.Fprintln(stdout, string(v))
		return err
	}
	b, _ := json.MarshalIndent(x, "", "  ")
	_, err := fmt.Fprintln(stdout, string(b))
	return err
}

// ---- serve ----

func serveCmd(args []string) error {
	f, c := newFlags("serve")
	quiet := f.Bool("quiet", false, "do not log connection state changes")
	if pos, err := parse(f, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return fmt.Errorf("serve takes no arguments (got %q)", pos[0])
	}
	if *c.json {
		return errors.New("serve does not support --json")
	}
	dir, err := filepath.Abs(*c.dataDir)
	if err != nil {
		return err
	}
	if _, err := envelope(native.Start(mustJSON(map[string]any{"data_dir": dir}))); err != nil {
		return fmt.Errorf("start kernel: %w", err)
	}
	stop := func() { _, _ = envelope(native.Call(`{"action":"stop"}`)) }
	l, err := control.Listen(dir)
	if err != nil {
		stop()
		return fmt.Errorf("control channel: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Printf("localsubproxy serve: data dir %s, control %s", dir, control.Address(dir))
	done := make(chan error, 1)
	go func() { done <- control.Serve(ctx, l, native.Call) }()
	if !*quiet {
		go watch(ctx)
	}
	select {
	case <-ctx.Done():
		err = <-done
	case err = <-done:
	}
	cancel()
	stop()
	log.Printf("localsubproxy serve: stopped")
	return err
}

// watch logs each connection's state when it changes.
func watch(ctx context.Context) {
	last := map[string]string{}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		st, err := envelope(native.Call(`{"action":"status"}`))
		if err == nil {
			var s statusView
			_ = json.Unmarshal(st, &s)
			seen := map[string]bool{}
			for _, cn := range s.Connections {
				seen[cn.ID] = true
				line := cn.label()
				if cn.LastError != "" && !cn.Connected {
					line += " (" + cn.LastError + ")"
				}
				if last[cn.ID] != line {
					log.Printf("connection %s %s: %s", cn.ID, cn.SaaSName, line)
					last[cn.ID] = line
				}
			}
			for id := range last {
				if !seen[id] {
					log.Printf("connection %s: removed", id)
					delete(last, id)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func envelope(raw string) (json.RawMessage, error) {
	var e struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return nil, err
	}
	if !e.OK {
		if e.Error == "" {
			e.Error = "request failed"
		}
		return nil, errors.New(e.Error)
	}
	return e.Data, nil
}

// ---- status / connections ----

type connView struct {
	ID           string   `json:"id"`
	SaaSName     string   `json:"saas_name"`
	DisplayURL   string   `json:"display_url"`
	Issuer       string   `json:"issuer"`
	Tenant       string   `json:"tenant"`
	DeviceID     string   `json:"device_id"`
	State        string   `json:"state"`
	Connected    bool     `json:"connected"`
	Paused       bool     `json:"paused"`
	RunningTasks int      `json:"running_tasks"`
	Allow        []string `json:"allow"`
	LastError    string   `json:"last_error"`
}

// label never says "online" unless the LMBP session is actually connected.
func (c connView) label() string {
	switch {
	case c.Paused:
		return "paused"
	case c.Connected:
		return "online"
	case c.State == "" || c.State == "connected" || c.State == "online":
		return "offline"
	default:
		return c.State
	}
}

type statusView struct {
	Connections  []connView `json:"connections"`
	PairingState string     `json:"pairing_state"`
	Warning      string     `json:"warning"`
	StorageWarn  string     `json:"adapter_storage_warning"`
}

func statusCmd(args []string) error {
	f, c := newFlags("status")
	if pos, err := parse(f, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return fmt.Errorf("status takes no arguments (got %q)", pos[0])
	}
	raw, err := call(c, map[string]any{"action": "status"})
	if err != nil {
		return err
	}
	if *c.json {
		return printJSON(raw)
	}
	var s statusView
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	online := 0
	for _, cn := range s.Connections {
		if cn.Connected && !cn.Paused {
			online++
		}
	}
	ad, err := call(c, map[string]any{"action": "list_adapters"})
	if err != nil {
		return err
	}
	var al struct {
		Adapters []connector.AdapterView `json:"adapters"`
	}
	_ = json.Unmarshal(ad, &al)
	healthy := 0
	for _, a := range al.Adapters {
		if a.Health == "ready" {
			healthy++
		}
	}
	fmt.Fprintf(stdout, "kernel:       running (data dir %s)\n", *c.dataDir)
	fmt.Fprintf(stdout, "connections:  %d (%d online)\n", len(s.Connections), online)
	fmt.Fprintf(stdout, "schemes:      %d (%d healthy)\n", len(al.Adapters), healthy)
	if s.PairingState == "consent" || s.PairingState == "approving" || s.PairingState == "pending" {
		fmt.Fprintf(stdout, "pairing:      %s\n", s.PairingState)
	}
	for _, w := range []string{s.Warning, s.StorageWarn} {
		if w != "" {
			fmt.Fprintf(stdout, "warning:      %s\n", w)
		}
	}
	if len(s.Connections) > 0 {
		fmt.Fprintln(stdout)
		printConns(s.Connections)
	}
	return nil
}

func printConns(cs []connView) {
	w := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSAAS\tSTATUS\tTASKS\tMODELS\tURL")
	for _, c := range cs {
		models := strings.Join(c.Allow, ",")
		if models == "" {
			models = "(none)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", c.ID, c.SaaSName, c.label(), c.RunningTasks, models, c.DisplayURL)
	}
	_ = w.Flush()
}

func connectionsCmd(args []string) error {
	f, c := newFlags("connections")
	if pos, err := parse(f, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return fmt.Errorf("connections takes no arguments (got %q)", pos[0])
	}
	raw, err := call(c, map[string]any{"action": "connections_list"})
	if err != nil {
		return err
	}
	if *c.json {
		return printJSON(raw)
	}
	var s statusView
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	if len(s.Connections) == 0 {
		fmt.Fprintln(stdout, "no connections (pair one with: localsubproxy pair <url>)")
		return nil
	}
	printConns(s.Connections)
	return nil
}

// ---- revoke / models ----

func revokeCmd(args []string) error {
	f, c := newFlags("revoke")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: localsubproxy revoke <connection-id>")
	}
	raw, err := call(c, map[string]any{"action": "connection_revoke", "connection_id": pos[0]})
	if err != nil {
		return err
	}
	if *c.json {
		return printJSON(raw)
	}
	fmt.Fprintf(stdout, "revoked %s (device keys deleted; pair again to reconnect)\n", pos[0])
	var w struct {
		Warning string `json:"warning"`
	}
	if json.Unmarshal(raw, &w) == nil && w.Warning != "" {
		fmt.Fprintf(stdout, "warning: %s\n", w.Warning)
	}
	return nil
}

func modelsCmd(args []string) error {
	if len(args) == 0 || args[0] != "set" {
		return errors.New("usage: localsubproxy models set <connection-id> <scheme/model,...|*|none>")
	}
	f, c := newFlags("models set")
	pos, err := parse(f, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: localsubproxy models set <connection-id> <scheme/model,...|*|none>")
	}
	allow := []string{}
	if pos[1] != "none" {
		allow = split(pos[1])
	}
	raw, err := call(c, map[string]any{"action": "connection_policy_set", "connection_id": pos[0], "policy": map[string]any{"allow": allow}})
	if err != nil {
		return err
	}
	if *c.json {
		return printJSON(raw)
	}
	if len(allow) == 0 {
		fmt.Fprintf(stdout, "%s: no models allowed\n", pos[0])
	} else {
		fmt.Fprintf(stdout, "%s: allowed %s\n", pos[0], strings.Join(allow, ","))
	}
	return nil
}

// ---- pair ----

func pairCmd(args []string) error {
	f, c := newFlags("pair")
	allowFlag := f.String("allow", "", "models this SaaS may use: scheme/model,... or * (asked interactively if omitted)")
	yes := f.Bool("yes", false, "approve the consent without prompting (automation)")
	conc := f.Int("max-concurrency", 0, "tasks served in parallel for this SaaS (0 = default)")
	timeout := f.Duration("timeout", 15*time.Minute, "give up waiting for the SaaS-side confirmation after this")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: localsubproxy pair <https://saas.example | lmbp://pair?...> [--allow ...] [--yes]")
	}
	target := pos[0]
	req := map[string]any{"action": "pair_prepare"}
	switch {
	case strings.HasPrefix(target, "lmbp://"):
		req["link"] = target
	case strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://"):
		req["remote_url"] = target
	default:
		return fmt.Errorf("unsupported pairing target %q: use an https:// SaaS URL or an lmbp://pair link", target)
	}
	cl, err := control.Connect(*c.dataDir)
	if err != nil {
		return err
	}
	defer cl.Close()
	raw, err := do(cl, req)
	if err != nil {
		return err
	}
	var prep struct {
		PairID    string        `json:"pair_id"`
		Consent   clientConsent `json:"consent"`
		Available []string      `json:"available_models"`
	}
	if err := json.Unmarshal(raw, &prep); err != nil {
		return err
	}
	in := bufio.NewReader(stdin)
	if !*c.json {
		prep.Consent.print()
		fmt.Fprintf(stdout, "Local models:    %s\n", orNone(prep.Available))
	}
	if !*yes && !confirmFrom(in, "Approve this SaaS? [y/N] ") {
		_, _ = do(cl, map[string]any{"action": "pair_deny", "pair_id": prep.PairID})
		return errors.New("pairing denied")
	}
	allowS := *allowFlag
	if allowS == "" && !*yes {
		fmt.Fprint(stdout, "Models to allow (comma separated, * for all, empty for none): ")
		line, _ := in.ReadString('\n')
		allowS = strings.TrimSpace(line)
	}
	allow := split(allowS)
	if allow == nil {
		allow = []string{}
	}
	ap, err := do(cl, map[string]any{"action": "pair_approve", "pair_id": prep.PairID, "allow": allow, "max_concurrency": *conc})
	if err != nil {
		_, _ = do(cl, map[string]any{"action": "pair_cancel", "pair_id": prep.PairID})
		return err
	}
	var code struct {
		UserCode string `json:"user_code"`
		LinkURL  string `json:"link_url"`
	}
	_ = json.Unmarshal(ap, &code)
	if *c.json {
		_ = printJSON(ap)
	} else {
		fmt.Fprintf(stdout, "\nConfirm on the SaaS side:\n  code: %s\n", code.UserCode)
		if code.LinkURL != "" {
			fmt.Fprintf(stdout, "  link: %s\n", code.LinkURL)
		}
		fmt.Fprintln(stdout, "Waiting for confirmation (Ctrl-C cancels)...")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, *timeout)
	defer cancel2()
	for {
		st, err := do(cl, map[string]any{"action": "pair_status", "pair_id": prep.PairID})
		if err != nil {
			return err
		}
		var ps struct {
			State        string `json:"state"`
			Error        string `json:"error"`
			ConnectionID string `json:"connection_id"`
		}
		_ = json.Unmarshal(st, &ps)
		switch ps.State {
		case "paired":
			if *c.json {
				return printJSON(st)
			}
			fmt.Fprintf(stdout, "paired: connection %s (%s)\n", ps.ConnectionID, prep.Consent.SaaS.Name)
			return nil
		case "failed", "denied", "cancelled", "idle":
			if ps.Error == "" {
				ps.Error = ps.State
			}
			return fmt.Errorf("pairing %s: %s", ps.State, ps.Error)
		}
		select {
		case <-ctx.Done():
			_, _ = do(cl, map[string]any{"action": "pair_cancel", "pair_id": prep.PairID})
			return errors.New("pairing cancelled")
		case <-time.After(time.Second):
		}
	}
}

type clientConsent struct {
	SaaS struct {
		Name        string `json:"name"`
		HomepageURI string `json:"homepage_uri"`
	} `json:"saas"`
	Gateway         string   `json:"gateway"`
	Tenant          string   `json:"tenant"`
	RequestedModels []string `json:"requested_models"`
	Verified        bool     `json:"verified"`
}

func (c clientConsent) print() {
	fmt.Fprintf(stdout, "SaaS:            %s\n", c.SaaS.Name)
	if c.SaaS.HomepageURI != "" {
		fmt.Fprintf(stdout, "Homepage:        %s\n", c.SaaS.HomepageURI)
	}
	fmt.Fprintf(stdout, "Gateway:         %s (tenant %s)\n", c.Gateway, c.Tenant)
	fmt.Fprintf(stdout, "Signed metadata: %v\n", c.Verified)
	fmt.Fprintf(stdout, "Requests models: %s\n", orNone(c.RequestedModels))
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}

func confirmFrom(in *bufio.Reader, prompt string) bool {
	fmt.Fprint(stdout, prompt)
	line, _ := in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// ---- scheme ----

var schemeTypes = map[string]string{
	"ollama":    "ollama_http",
	"openai":    "openai_compatible",
	"anthropic": "anthropic_compatible",
	"codex":     "codex_app_server",
}

func schemeType(t string) (string, error) {
	if full, ok := schemeTypes[t]; ok {
		return full, nil
	}
	for _, full := range schemeTypes {
		if t == full {
			return full, nil
		}
	}
	return "", fmt.Errorf("unsupported scheme type %q (ollama, openai, anthropic or codex)", t)
}

// rejectKeyArgs refuses an API key on the command line (visible in ps and
// shell history).
func rejectKeyArgs(args []string) error {
	for _, a := range args {
		n := strings.TrimLeft(a, "-")
		if strings.HasPrefix(a, "-") && (n == "api-key" || strings.HasPrefix(n, "api-key=") || n == "key" || strings.HasPrefix(n, "key=")) {
			return errors.New("API keys are never accepted as arguments: use --api-key-env VAR or --api-key-stdin")
		}
	}
	return nil
}

type schemeFlags struct {
	typ, id, name, baseURL, models, codexBin, caps, keyEnv, provider, effort *string
	keyStdin, enable, disable, def                                           *bool
	conc, ctxWin, maxOut                                                     *int
}

func addSchemeFlags(f *flag.FlagSet, add bool) schemeFlags {
	s := schemeFlags{
		id:       f.String("id", "", "scheme ID (letters, digits, - or _; default derived from --type)"),
		name:     f.String("name", "", fmt.Sprintf("display name (at most %d bytes; default from the first model)", connector.MaxSchemeNameBytes)),
		baseURL:  f.String("base-url", "", "API base URL (ollama, openai, anthropic)"),
		models:   f.String("models", "", "comma separated model IDs"),
		codexBin: f.String("codex-bin", "", "Codex CLI executable (codex)"),
		caps:     f.String("capabilities", "", "comma separated capability flags: tools,vision,audio"),
		keyEnv:   f.String("api-key-env", "", "read the API key from this environment variable"),
		keyStdin: f.Bool("api-key-stdin", false, "read the API key from the first line of stdin"),
		provider: f.String("provider", "", "provider label (optional)"),
		effort:   f.String("reasoning-effort", "", "off, low, medium or high"),
		conc:     f.Int("concurrency", 0, "parallel tasks on this scheme"),
		ctxWin:   f.Int("context-window", 0, "context window tokens"),
		maxOut:   f.Int("max-output-tokens", 0, "maximum output tokens"),
		def:      f.Bool("default", false, "make this the default scheme"),
	}
	if add {
		s.typ = f.String("type", "", "ollama, openai, anthropic or codex (required)")
	} else {
		s.enable = f.Bool("enable", false, "enable the scheme")
		s.disable = f.Bool("disable", false, "disable the scheme")
	}
	return s
}

func (s schemeFlags) apiKey() (string, bool, error) {
	if *s.keyEnv != "" && *s.keyStdin {
		return "", false, errors.New("use either --api-key-env or --api-key-stdin")
	}
	if *s.keyEnv != "" {
		v, ok := os.LookupEnv(*s.keyEnv)
		if !ok || v == "" {
			return "", false, fmt.Errorf("environment variable %s is not set", *s.keyEnv)
		}
		return v, true, nil
	}
	if *s.keyStdin {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", false, errors.New("no API key on stdin")
		}
		return strings.TrimSpace(line), true, nil
	}
	return "", false, nil
}

func checkName(n string) error {
	if len(strings.TrimSpace(n)) > connector.MaxSchemeNameBytes {
		return fmt.Errorf("scheme name is %d bytes; the maximum is %d", len(strings.TrimSpace(n)), connector.MaxSchemeNameBytes)
	}
	if !utf8.ValidString(n) {
		return errors.New("scheme name is not valid UTF-8")
	}
	return nil
}

func schemeCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: localsubproxy scheme add|list|update|rm|test ...")
	}
	sub, rest := args[0], args[1:]
	if err := rejectKeyArgs(rest); err != nil {
		return err
	}
	switch sub {
	case "add":
		return schemeAdd(rest)
	case "list", "ls":
		return schemeList(rest)
	case "update":
		return schemeUpdate(rest)
	case "rm", "remove":
		return schemeRm(rest)
	case "test":
		return schemeTest(rest)
	}
	return fmt.Errorf("unsupported scheme subcommand %q (add, list, update, rm, test)", sub)
}

func listAdapters(c common) ([]connector.AdapterView, error) {
	raw, err := call(c, map[string]any{"action": "list_adapters"})
	if err != nil {
		return nil, err
	}
	var al struct {
		Adapters []connector.AdapterView `json:"adapters"`
	}
	err = json.Unmarshal(raw, &al)
	return al.Adapters, err
}

func schemeAdd(args []string) error {
	f, c := newFlags("scheme add")
	s := addSchemeFlags(f, true)
	if pos, err := parse(f, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q", pos[0])
	}
	typ, err := schemeType(*s.typ)
	if err != nil {
		return err
	}
	if err := checkName(*s.name); err != nil {
		return err
	}
	a := connector.AdapterConfig{Type: typ, ID: *s.id, Name: strings.TrimSpace(*s.name), BaseURL: *s.baseURL, Models: split(*s.models),
		CodexBin: *s.codexBin, Capabilities: split(*s.caps), Provider: *s.provider, ReasoningEffort: *s.effort,
		Concurrency: *s.conc, ContextWindow: *s.ctxWin, MaxOutputTokens: *s.maxOut, IsDefault: *s.def, Enabled: true}
	key, hasKey, err := s.apiKey()
	if err != nil {
		return err
	}
	switch typ {
	case "openai_compatible", "anthropic_compatible":
		if !hasKey {
			return errors.New("this scheme type needs an API key: --api-key-env VAR or --api-key-stdin")
		}
	default:
		if hasKey {
			return fmt.Errorf("%s schemes do not take an API key", *s.typ)
		}
	}
	a.APIKey = key
	if typ == "ollama_http" && a.BaseURL == "" {
		a.BaseURL = "http://127.0.0.1:11434"
	}
	if typ == "codex_app_server" && a.CodexBin == "" {
		a.CodexBin = "codex"
	}
	if a.ID == "" {
		existing, err := listAdapters(c)
		if err != nil {
			return err
		}
		a.ID = nextID(strings.SplitN(typ, "_", 2)[0], existing)
	}
	raw, err := call(c, map[string]any{"action": "create_adapter", "adapter": a})
	if err != nil {
		return err
	}
	if *c.json {
		return printJSON(raw)
	}
	var v connector.AdapterView
	_ = json.Unmarshal(raw, &v)
	fmt.Fprintf(stdout, "added scheme %s (%s, %s): models %s\n", v.ID, v.Name, v.Type, strings.Join(v.Models, ","))
	return nil
}

func nextID(base string, existing []connector.AdapterView) string {
	taken := map[string]bool{}
	for _, a := range existing {
		taken[a.ID] = true
	}
	id := base
	for n := 2; taken[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

func schemeList(args []string) error {
	f, c := newFlags("scheme list")
	if pos, err := parse(f, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q", pos[0])
	}
	if *c.json {
		raw, err := call(c, map[string]any{"action": "list_adapters"})
		if err != nil {
			return err
		}
		return printJSON(raw)
	}
	as, err := listAdapters(c)
	if err != nil {
		return err
	}
	if len(as) == 0 {
		fmt.Fprintln(stdout, "no schemes (add one with: localsubproxy scheme add --type ...)")
		return nil
	}
	sort.Slice(as, func(i, j int) bool { return as[i].ID < as[j].ID })
	w := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tTYPE\tMODELS\tENABLED\tHEALTH")
	for _, a := range as {
		h := a.Health
		if h == "" {
			h = "unknown"
		}
		if a.HealthError != "" {
			h += ": " + a.HealthError
		}
		id := a.ID
		if a.IsDefault {
			id += "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\t%s\n", id, a.Name, a.Type, strings.Join(a.Models, ","), a.Enabled, h)
	}
	return w.Flush()
}

func schemeUpdate(args []string) error {
	f, c := newFlags("scheme update")
	s := addSchemeFlags(f, false)
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: localsubproxy scheme update <id> [--name ...] [--models ...] ...")
	}
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	patch := map[string]any{"id": pos[0]}
	if set["id"] {
		return errors.New("a scheme ID cannot be changed; add a new scheme and remove the old one")
	}
	if set["name"] {
		if err := checkName(*s.name); err != nil {
			return err
		}
		patch["name"] = strings.TrimSpace(*s.name)
	}
	str := map[string]*string{"base-url": s.baseURL, "codex-bin": s.codexBin, "provider": s.provider, "reasoning-effort": s.effort}
	keys := map[string]string{"base-url": "base_url", "codex-bin": "codex_bin", "provider": "provider", "reasoning-effort": "reasoning_effort"}
	for fl, p := range str {
		if set[fl] {
			patch[keys[fl]] = *p
		}
	}
	if set["models"] {
		patch["models"] = split(*s.models)
	}
	if set["capabilities"] {
		patch["capabilities"] = split(*s.caps)
	}
	for fl, p := range map[string]*int{"concurrency": s.conc, "context-window": s.ctxWin, "max-output-tokens": s.maxOut} {
		if set[fl] {
			patch[strings.ReplaceAll(fl, "-", "_")] = *p
		}
	}
	if set["enable"] && set["disable"] {
		return errors.New("use either --enable or --disable")
	}
	if set["enable"] || set["disable"] {
		patch["enabled"] = set["enable"]
	}
	if set["default"] {
		patch["is_default"] = *s.def
	}
	key, hasKey, err := s.apiKey()
	if err != nil {
		return err
	}
	if hasKey {
		patch["api_key"] = key
	}
	if len(patch) == 1 {
		return errors.New("nothing to update")
	}
	raw, err := call(c, map[string]any{"action": "update_adapter", "adapter": patch})
	if err != nil {
		return err
	}
	if *c.json {
		return printJSON(raw)
	}
	var v connector.AdapterView
	_ = json.Unmarshal(raw, &v)
	fmt.Fprintf(stdout, "updated scheme %s (%s): models %s, enabled %v\n", v.ID, v.Name, strings.Join(v.Models, ","), v.Enabled)
	return nil
}

func schemeRm(args []string) error {
	f, c := newFlags("scheme rm")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: localsubproxy scheme rm <id>")
	}
	raw, err := call(c, map[string]any{"action": "delete_adapter", "id": pos[0]})
	if err != nil {
		return err
	}
	if *c.json {
		return printJSON(raw)
	}
	fmt.Fprintf(stdout, "removed scheme %s\n", pos[0])
	return nil
}

func schemeTest(args []string) error {
	f, c := newFlags("scheme test")
	pos, err := parse(f, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: localsubproxy scheme test <id>")
	}
	as, err := listAdapters(c)
	if err != nil {
		return err
	}
	var a *connector.AdapterView
	for i := range as {
		if as[i].ID == pos[0] {
			a = &as[i]
		}
	}
	if a == nil {
		return fmt.Errorf("scheme %q not found", pos[0])
	}
	if a.HealthError != "" && a.Health == "unavailable" && strings.HasPrefix(a.HealthError, "invalid configuration") {
		return fmt.Errorf("scheme %s cannot be tested: %s (fix it with scheme update)", a.ID, a.HealthError)
	}
	cfg := connector.AdapterConfig{ID: a.ID, Type: a.Type, Name: a.Name, Provider: a.Provider, BaseURL: a.BaseURL, CodexBin: a.CodexBin,
		Models: a.Models, ContextWindow: a.ContextWindow, MaxOutputTokens: a.MaxOutputTokens, ReasoningEffort: a.ReasoningEffort,
		Capabilities: a.ConfiguredCapabilities, Concurrency: a.ConfiguredConcurrency, Enabled: true} // the stored key is reused
	raw, err := call(c, map[string]any{"action": "test_adapter", "adapter": cfg})
	if err != nil {
		return err
	}
	if *c.json {
		return printJSON(raw)
	}
	var h connector.AdapterHealth
	_ = json.Unmarshal(raw, &h)
	fmt.Fprintf(stdout, "%s: health %s, auth %s, models %s\n", a.ID, h.Health, h.AuthState, orNone(h.Models))
	if h.Error != "" {
		fmt.Fprintf(stdout, "error: %s\n", h.Error)
	}
	if h.Health != "ready" {
		return errors.New("scheme test failed")
	}
	return nil
}
