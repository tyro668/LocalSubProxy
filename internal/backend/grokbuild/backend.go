// Package grokbuild bridges the official Grok Build headless stream.
package grokbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/cliruntime"
	"github.com/tyro668/avora/internal/lmbp"
)

type Adapter struct {
	config      backend.Config
	userHomeDir func() (string, error)
}

func Definition() backend.Definition {
	return backend.Definition{
		Type: "grok_build", Name: "Grok Build", Label: "Grok Build（CLI）", Short: "grok", Executable: "grok", LoginCommand: "grok login", Cloud: true,
		Efforts: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, EffortAliases: map[string]string{"off": ""}, DefaultConcurrency: 1, MaxConcurrency: 1,
		Validate: func(a *backend.Config, required bool) error {
			return cliruntime.Validate(a, required, "grok", "Grok Build")
		},
		New: func(a backend.Config, d backend.Dependencies) backend.Backend {
			homeDir := d.UserHomeDir
			if homeDir == nil {
				homeDir = os.UserHomeDir
			}
			return &Adapter{config: a, userHomeDir: homeDir}
		},
	}
}

// The CLI owns its auth store. An isolated GROK_HOME avoids loading user hooks,
// MCP, skills, instructions, or remembered sessions; GROK_AUTH_PATH points the
// official CLI at its existing credential store without reading/copying tokens.
func (a *Adapter) prepare() (cliruntime.Options, func(), error) {
	bin, err := cliruntime.Resolve(a.config.Executable, "Grok Build")
	if err != nil {
		return cliruntime.Options{}, nil, err
	}
	dir, err := cliruntime.Workspace("grok")
	if err != nil {
		return cliruntime.Options{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	user, err := a.userHomeDir()
	if err != nil {
		cleanup()
		return cliruntime.Options{}, nil, err
	}
	source := os.Getenv("GROK_HOME")
	if source == "" {
		source = filepath.Join(user, ".grok")
	}
	auth := os.Getenv("GROK_AUTH_PATH")
	if auth == "" {
		auth = filepath.Join(source, "auth.json")
	}
	auth, err = filepath.Abs(auth)
	if err != nil {
		cleanup()
		return cliruntime.Options{}, nil, err
	}
	home := filepath.Join(dir, "runtime")
	if err = os.MkdirAll(home, 0700); err != nil {
		cleanup()
		return cliruntime.Options{}, nil, err
	}
	config := []byte("[cli]\nauto_update = false\n[memory]\nenabled = false\n[subagents]\nenabled = false\n[goal]\nenabled = false\n[session]\nload_envrc = false\n")
	if err = os.WriteFile(filepath.Join(home, "config.toml"), config, 0600); err != nil {
		cleanup()
		return cliruntime.Options{}, nil, err
	}
	env := cliruntime.Environment()
	for k, v := range map[string]string{"GROK_HOME": home, "GROK_AUTH_PATH": auth, "GROK_DISABLE_AUTOUPDATER": "1", "GROK_MEMORY": "0", "GROK_SUBAGENTS": "0", "GROK_GOAL": "0", "GROK_MANAGED_MCPS_ENABLED": "0", "GROK_MANAGED_MCP_GATEWAY_TOOLS_ENABLED": "0"} {
		env = cliruntime.SetEnv(env, k, v)
	}
	for _, vendor := range []string{"CURSOR", "CLAUDE", "CODEX"} {
		for _, kind := range []string{"SKILLS", "RULES", "AGENTS", "MCPS", "HOOKS"} {
			env = cliruntime.SetEnv(env, "GROK_"+vendor+"_"+kind+"_ENABLED", "0")
		}
	}
	return cliruntime.Options{Executable: bin, Dir: dir, Env: env, Args: []string{"--no-auto-update"}}, cleanup, nil
}
func (a *Adapter) Models(ctx context.Context) ([]string, error) {
	o, close, err := a.prepare()
	if err != nil {
		return nil, err
	}
	defer close()
	o.Args = append(o.Args, "models")
	data, err := cliruntime.Output(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("Grok model discovery failed: %w", err)
	}
	return parseModels(string(data))
}
func parseModels(text string) ([]string, error) {
	if strings.Contains(strings.ToLower(text), "not authenticated") {
		return nil, backend.Failure("AUTH_REQUIRED: run grok login in a terminal")
	}
	var models []string
	listing := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "Available models:" {
			listing = true
			continue
		}
		if !listing || len(line) < 2 || (line[0] != '*' && line[0] != '-') {
			continue
		}
		fields := strings.Fields(line[1:])
		if len(fields) > 0 {
			models = append(models, fields[0])
		}
	}
	if len(models) == 0 {
		return nil, backend.Failure("PROTOCOL_MISMATCH: Grok returned no recognizable model catalog")
	}
	return models, nil
}
func (a *Adapter) Probe(ctx context.Context) backend.Health {
	return backend.ProbeCLI(ctx, a.config, a.Models)
}
func (a *Adapter) Execute(ctx context.Context, r lmbp.TaskRun, out *backend.Emitter) (*lmbp.TaskDone, error) {
	effort := Definition().Effort(a.config.ReasoningEffort)
	if effort != "" {
		catalog, err := a.Catalog(ctx)
		if err != nil {
			return nil, err
		}
		if _, err = backend.ResolveEffort(catalog, r.Model, effort); err != nil {
			return nil, err
		}
	}
	o, close, err := a.prepare()
	if err != nil {
		return nil, err
	}
	defer close()
	if err = cliruntime.CheckFlags(ctx, o, "--prompt-file", "--tools", "--disallowed-tools", "--agent", "--verbatim", "--permission-mode", "--disable-web-search"); err != nil {
		return nil, err
	}
	agent := filepath.Join(o.Dir, "agent.md")
	if err = os.WriteFile(agent, []byte("---\nname: avora-inference\ndescription: Text inference without local actions\nagentsMd: false\ndiscoverSkills: false\ninheritSkills: false\n---\nRespond to the supplied conversation using text only.\n"), 0600); err != nil {
		return nil, err
	}
	prompt := filepath.Join(o.Dir, "prompt.txt")
	if err = os.WriteFile(prompt, []byte(backend.TextInput(r.Messages)), 0600); err != nil {
		return nil, err
	}
	// Empty or unknown allowlists inherit tools on some CLI versions. Select
	// one known tool, then deny it and the MCP entry points that Grok keeps
	// outside the allowlist: the resulting toolset is empty.
	// The stream also verifies that no tools were actually exposed.
	o.Args = append(o.Args, "--agent", agent, "--tools", "read_file", "--disallowed-tools", "read_file,search_tool,use_tool", "--no-plan", "--no-subagents", "--disable-web-search", "--permission-mode", "dontAsk", "--sandbox", "read-only", "--max-turns", "1", "--verbatim", "--model", r.Model, "--output-format", "streaming-json", "--prompt-file", prompt)
	if effort != "" {
		o.Args = append(o.Args, "--reasoning-effort", effort)
	}
	stream := newStream(out)
	err = cliruntime.Lines(ctx, o, func(line []byte, _ io.WriteCloser) error { return stream.consume(line) })
	if err != nil {
		return nil, err
	}
	if stream.done == nil {
		return nil, backend.Failure("PROTOCOL_MISMATCH: Grok stream ended without a terminal event")
	}
	return stream.done, nil
}

type stream struct {
	out      *backend.Emitter
	done     *lmbp.TaskDone
	usage    *lmbp.Usage
	manifest bool // an empty available_commands tool list was seen
}

func newStream(out *backend.Emitter) *stream { return &stream{out: out} }

// consume handles one headless streaming-json line. Text and the terminal
// event count only after Grok has announced an empty tool inventory; tool
// activity of any kind aborts the task.
func (s *stream) consume(line []byte) error {
	var e struct {
		Type    string    `json:"type"`
		Data    string    `json:"data"`
		Message string    `json:"message"`
		Stop    string    `json:"stopReason"`
		Tools   *[]string `json:"tools"`
		Usage   *struct {
			Input   int `json:"input_tokens"`
			Output  int `json:"output_tokens"`
			Cached  int `json:"cache_read_input_tokens"`
			Created int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return fmt.Errorf("invalid Grok stream: %w", err)
	}
	if s.done != nil {
		return backend.Failure("PROTOCOL_MISMATCH: Grok emitted data after completion")
	}
	if e.Usage != nil {
		s.usage = &lmbp.Usage{PromptTokens: e.Usage.Input + e.Usage.Cached + e.Usage.Created, CompletionTokens: e.Usage.Output}
	}
	switch e.Type {
	case "thought", "usage", "auto_compact_started", "auto_compact_completed", "image_compressed":
		return nil
	case "available_commands":
		if e.Tools == nil {
			return backend.Failure("POLICY_DENIED: Grok sent a tool inventory without a tools list")
		}
		if len(*e.Tools) > 0 {
			return backend.Failure("POLICY_DENIED: Grok exposed tools despite the inference-only policy: " + strings.Join(*e.Tools, ", "))
		}
		s.manifest = true
		return nil
	case "tool_call", "tool_call_update", "plan":
		return backend.Failure("POLICY_DENIED: Grok attempted a local action")
	case "error":
		return backend.CLIFailure("Grok", e.Message)
	case "max_turns_reached":
		return backend.Failure("PROTOCOL_MISMATCH: Grok reached the agent turn limit")
	case "text", "end":
		if !s.manifest {
			return backend.Failure("POLICY_DENIED: Grok did not confirm an empty tool inventory before answering")
		}
		if e.Type == "text" {
			return s.out.Text(e.Data)
		}
		done, err := backend.Finish("Grok", e.Stop, s.usage)
		if err != nil {
			return err
		}
		s.done = done
		return nil
	}
	return errors.New("unrecognized Grok stream event: " + e.Type)
}

func (a *Adapter) LoginStatus(ctx context.Context) backend.LoginStatus {
	return backend.LoginFromModels(ctx, Definition(), a.Models)
}
