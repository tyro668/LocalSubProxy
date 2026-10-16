package main

// Real-path headless test: the built binary runs the mock SaaS gateway
// (`localsubproxy server`), a fake model, and `localsubproxy serve`; the CLI
// clients pair, add a scheme, change the allowed models and revoke while a
// SaaS-side request goes through the gateway to the local model.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func buildBin(t *testing.T, dir, pkg, name string) string {
	out := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = "../.."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return out
}

func startBG(t *testing.T, logf string, bin string, args ...string) *exec.Cmd {
	f, err := os.Create(logf)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
		f.Close()
	})
	return cmd
}

func waitFor(t *testing.T, what string, ok func() bool) {
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestHeadlessEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds binaries and runs a mock gateway")
	}
	tmp := t.TempDir()
	bin := buildBin(t, tmp, "./cmd/localsubproxy", "localsubproxy")
	mock := buildBin(t, tmp, "./mock/cmd/lspmock", "lspmock")
	modelPort, gwPort := freePort(t), freePort(t)
	gw := fmt.Sprintf("http://127.0.0.1:%d", gwPort)
	const tenantKey = "e2e-tenant-key-0123456789abcdef-0123456789"
	t.Setenv("LSP_TENANT_KEY_ACME", tenantKey)
	t.Setenv("LSP_SIGNING_SEED", strings.Repeat("ab", 32))
	prof := filepath.Join(tmp, "profiles.json")
	_ = os.WriteFile(prof, []byte(`{"acme":{"api_key_env":"LSP_TENANT_KEY_ACME","display_name":"Acme E2E","homepage_uri":"`+gw+`","pairing_uri":"`+gw+`/chat","requested_models":["qwen3-8b"]}}`), 0o600)
	startBG(t, filepath.Join(tmp, "model.log"), mock, "fakemodel", "-listen", fmt.Sprintf("127.0.0.1:%d", modelPort), "-models", "qwen3-8b")
	startBG(t, filepath.Join(tmp, "gw.log"), bin, "server", "-listen", fmt.Sprintf("127.0.0.1:%d", gwPort), "-db", filepath.Join(tmp, "gw.db"),
		"-tenants", "acme:LSP_TENANT_KEY_ACME", "-tenant-profiles", prof, "-issuer", gw)
	waitFor(t, "gateway", func() bool {
		r, err := http.Get(gw + "/.well-known/localsubproxy")
		if err == nil {
			r.Body.Close()
		}
		return err == nil && r.StatusCode < 500
	})

	// short path: unix socket paths are limited to ~104 bytes
	data, err := os.MkdirTemp("", "lspe2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	serveLog := filepath.Join(tmp, "serve.log")
	startBG(t, serveLog, bin, "serve", "--data-dir", data)

	run := func(stdin string, args ...string) (string, error) {
		cmd := exec.Command(bin, append(args, "--data-dir", data)...)
		cmd.Stdin = strings.NewReader(stdin)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := run("", args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		t.Logf("$ localsubproxy %s\n%s", strings.Join(args, " "), out)
		return out
	}
	waitFor(t, "serve", func() bool { _, err := run("", "status"); return err == nil })

	// a second kernel on the same data dir is refused
	if out, err := exec.Command(bin, "serve", "--data-dir", data).CombinedOutput(); err == nil || !strings.Contains(string(out), "data directory in use") {
		t.Fatalf("second serve: %v %s", err, out)
	}
	// API keys never on argv
	if out, err := run("", "scheme", "add", "--type", "openai", "--api-key", "sk-x", "--models", "m"); err == nil || !strings.Contains(out, "never accepted as arguments") {
		t.Fatalf("argv key: %v %s", err, out)
	}
	if out, err := run("", "scheme", "add", "--type", "ollama", "--models", "m", "--name", strings.Repeat("x", 101)); err == nil || !strings.Contains(out, "maximum is 100") {
		t.Fatalf("long name: %v %s", err, out)
	}
	must("scheme", "add", "--type", "ollama", "--id", "local", "--base-url", fmt.Sprintf("http://127.0.0.1:%d", modelPort), "--models", "qwen3-8b")
	if out, err := run("sk-e2e\n", "scheme", "add", "--type", "openai", "--id", "cloud", "--api-key-stdin", "--base-url", "https://api.example.invalid/v1", "--models", "gpt-x"); err != nil {
		t.Fatalf("stdin key: %v %s", err, out)
	}
	list := must("scheme", "list")
	if !regexp.MustCompile(`local\*\s+qwen3-8b\s+ollama_http\s+qwen3-8b\s+true\s+ready`).MatchString(list) || !strings.Contains(list, "cloud") {
		t.Fatalf("scheme list:\n%s", list)
	}
	must("scheme", "test", "local")
	must("scheme", "rm", "cloud")

	// pair: the SaaS approves the printed code through its tenant API
	pr, pw := io.Pipe()
	pair := exec.Command(bin, "pair", "--data-dir", data, "--yes", "--allow", "local/qwen3-8b",
		fmt.Sprintf("lmbp://pair?v=1&gateway=%s&tenant=acme", strings.ReplaceAll(strings.ReplaceAll(gw, ":", "%3A"), "/", "%2F")))
	pair.Stdout, pair.Stderr = pw, pw
	if err := pair.Start(); err != nil {
		t.Fatal(err)
	}
	var pairOut bytes.Buffer
	code := ""
	sc := bufio.NewScanner(pr)
	codeRe := regexp.MustCompile(`code: (\S+)`)
	for code == "" && sc.Scan() {
		pairOut.WriteString(sc.Text() + "\n")
		if m := codeRe.FindStringSubmatch(sc.Text()); m != nil {
			code = m[1]
		}
	}
	if code == "" {
		t.Fatalf("no user code:\n%s", pairOut.String())
	}
	go func() {
		for sc.Scan() {
			pairOut.WriteString(sc.Text() + "\n")
		}
	}()
	tenant := func(method, path, body string) (int, []byte) {
		req, _ := http.NewRequest(method, gw+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tenantKey)
		req.Header.Set("X-End-User", "alice")
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, b
	}
	if st, b := tenant("POST", "/lmbp/v1/tenant/devices/approvals", `{"user_code":"`+code+`"}`); st != 200 {
		t.Fatalf("approve: %d %s", st, b)
	}
	if err := pair.Wait(); err != nil {
		t.Fatalf("pair: %v\n%s", err, pairOut.String())
	}
	pw.Close()
	if !strings.Contains(pairOut.String(), "paired: connection b_") {
		t.Fatalf("pair output:\n%s", pairOut.String())
	}
	t.Logf("pair output:\n%s", pairOut.String())

	var conns struct {
		Connections []connView `json:"connections"`
	}
	waitFor(t, "online", func() bool {
		out, err := run("", "connections", "--json")
		return err == nil && json.Unmarshal([]byte(out), &conns) == nil && len(conns.Connections) == 1 && conns.Connections[0].Connected
	})
	cn := conns.Connections[0]
	st := must("status")
	if !strings.Contains(st, "connections:  1 (1 online)") || !strings.Contains(st, cn.ID+"  Acme E2E") {
		t.Fatalf("status:\n%s", st)
	}

	chat := func() (int, string) {
		code, b := tenant("POST", "/v1/chat/completions", `{"model":"`+cn.DeviceID+`/local/qwen3-8b","messages":[{"role":"user","content":"hi"}]}`)
		return code, string(b)
	}
	if code, b := chat(); code != 200 || !strings.Contains(b, "qwen3-8b") {
		t.Fatalf("remote call: %d %s", code, b)
	}
	must("models", "set", cn.ID, "none")
	waitFor(t, "model withdrawn", func() bool { code, _ := chat(); return code != 200 })
	must("models", "set", cn.ID, "local/qwen3-8b")
	waitFor(t, "model back", func() bool { code, _ := chat(); return code == 200 })

	must("revoke", cn.ID)
	st = must("status")
	if !strings.Contains(st, "connections:  0 (0 online)") {
		t.Fatalf("status after revoke:\n%s", st)
	}
	if code, _ := chat(); code == 200 {
		t.Fatal("revoked device still serves")
	}
	if _, err := os.Stat(filepath.Join(data, "kernel.lock")); err != nil {
		t.Fatal(err)
	}
}
