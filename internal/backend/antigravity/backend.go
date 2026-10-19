// Package antigravity bridges the official agy stream-json interface.
package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tyro668/avora/internal/backend"
	"github.com/tyro668/avora/internal/backend/cliruntime"
	"github.com/tyro668/avora/internal/lmbp"
)

const agentName = "avora-inference"
const agentDefinition = `---
name: avora-inference
description: Text inference without filesystem, terminal, network, or delegation tools
tools:
  - finish
mainAgent: true
subagent: false
commandExecutionPolicy: off
mcpServers: []
skills: []
plugins: []
---
Answer the supplied conversation using text. No local actions are available.
`

type Adapter struct {
	config      backend.Config
	userHomeDir func() (string, error)
}

func Definition() backend.Definition {
	return backend.Definition{
		Type: "antigravity_cli", Name: "Antigravity CLI", Executable: "agy", DefaultConcurrency: 1, MaxConcurrency: 1,
		Validate: func(a *backend.Config, required bool) error {
			return cliruntime.Validate(a, required, "agy", "Antigravity")
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
func (a *Adapter) prepare() (cliruntime.Options, func(), error) {
	bin, err := cliruntime.Resolve(a.config.Executable, "Antigravity")
	if err != nil {
		return cliruntime.Options{}, nil, err
	}
	home, err := a.userHomeDir()
	if err != nil {
		return cliruntime.Options{}, nil, err
	}
	if err = checkCustomizations(home); err != nil {
		return cliruntime.Options{}, nil, err
	}
	dir, err := cliruntime.Workspace("antigravity")
	if err != nil {
		return cliruntime.Options{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	agents := filepath.Join(dir, ".agents", "agents")
	if err = os.MkdirAll(agents, 0700); err == nil {
		err = os.WriteFile(filepath.Join(agents, agentName+".md"), []byte(agentDefinition), 0600)
	}
	if err != nil {
		cleanup()
		return cliruntime.Options{}, nil, err
	}
	return cliruntime.Options{Executable: bin, Dir: dir, Env: cliruntime.Environment()}, cleanup, nil
}
func (a *Adapter) Models(ctx context.Context) ([]string, error) {
	models, err := a.Catalog(ctx)
	return backend.ModelIDs(models), err
}
func (a *Adapter) modelOutput(ctx context.Context) (string, error) {
	o, close, err := a.prepare()
	if err != nil {
		return "", err
	}
	defer close()
	if err = cliruntime.CheckFlags(ctx, o, "--agent", "--disable-slash-commands", "--input-format"); err != nil {
		return "", err
	}
	o.Args = []string{"models"}
	data, err := cliruntime.Output(ctx, o)
	if err != nil {
		return "", fmt.Errorf("Antigravity model discovery failed: %w", err)
	}
	return string(data), nil
}
func parseModels(text string) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		id, label, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && id != "" && label != "" && !strings.ContainsAny(id, " \r\n") && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, backend.Failure("AUTH_REQUIRED: run agy in a terminal and sign in; no model catalog was returned")
	}
	return ids, nil
}
func (a *Adapter) Probe(ctx context.Context) backend.Health {
	return backend.ProbeCLI(ctx, a.config, a.Models)
}
func (a *Adapter) Execute(ctx context.Context, r lmbp.TaskRun, out *backend.Emitter) (*lmbp.TaskDone, error) {
	effort := backend.NormalizeEffort("antigravity_cli", a.config.ReasoningEffort)
	model := r.Model
	if effort != "" {
		catalog, err := a.Catalog(ctx)
		if err != nil {
			return nil, err
		}
		model, err = backend.ResolveEffort(catalog, model, effort)
		if err != nil {
			return nil, err
		}
	}
	o, close, err := a.prepare()
	if err != nil {
		return nil, err
	}
	defer close()
	if err = cliruntime.CheckFlags(ctx, o, "--agent", "--disable-slash-commands", "--input-format"); err != nil {
		return nil, err
	}
	o.Args = []string{"--agent", agentName, "--disable-slash-commands", "--input-format", "stream-json", "--output-format", "stream-json", "--model", model, "--print-timeout", "10m"}
	prompt, err := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"content": backend.TextInput(r.Messages)}})
	if err != nil {
		return nil, err
	}
	s := &stream{out: out, dir: o.Dir}
	err = cliruntime.Lines(ctx, o, func(line []byte, in io.WriteCloser) error {
		initialized := s.initialized
		if err := s.consume(line); err != nil {
			return err
		}
		if !initialized && s.initialized {
			if _, err := in.Write(append(prompt, '\n')); err != nil {
				return err
			}
			return in.Close() // one request per process; never resume another tenant's session
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.done == nil {
		return nil, backend.Failure("PROTOCOL_MISMATCH: Antigravity stream ended without a result")
	}
	return s.done, nil
}

type stream struct {
	out         *backend.Emitter
	dir         string
	initialized bool
	session     string
	text        strings.Builder
	done        *lmbp.TaskDone
}

func (s *stream) consume(line []byte) error {
	var e struct {
		Event   string `json:"event"`
		Session string `json:"conversation_id"`
		Init    struct {
			Agent string `json:"agent"`
			Cwd   string `json:"cwd"`
		} `json:"init"`
		Step struct {
			Session string `json:"conversation_id"`
			Type    string `json:"step_type"`
			Tool    string `json:"tool_name"`
			Text    string `json:"text_delta"`
		} `json:"step_update"`
		Result struct {
			Session  string `json:"conversation_id"`
			Status   string `json:"status"`
			Response string `json:"response"`
			Error    string `json:"error"`
			Usage    *struct {
				Input  int `json:"input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return fmt.Errorf("invalid Antigravity stream: %w", err)
	}
	if s.done != nil {
		return backend.Failure("PROTOCOL_MISMATCH: Antigravity emitted data after completion")
	}
	switch e.Event {
	case "init":
		if s.initialized || e.Session == "" || e.Init.Agent != agentName {
			return backend.Failure("PROTOCOL_MISMATCH: Antigravity did not select the inference agent")
		}
		cwd, err := filepath.EvalSymlinks(e.Init.Cwd)
		if err != nil {
			return err
		}
		want, err := filepath.EvalSymlinks(s.dir)
		if err != nil {
			return err
		}
		if cwd != want {
			return backend.Failure("POLICY_DENIED: Antigravity selected an unexpected workspace")
		}
		// init.tools is the GLOBAL registry, not the selected agent's allowlist.
		// The private agent definition restricts it to the inert finish operation.
		s.initialized = true
		s.session = e.Session
	case "step_update":
		if !s.initialized || e.Step.Session != s.session {
			return backend.Failure("PROTOCOL_MISMATCH: unexpected Antigravity session")
		}
		switch e.Step.Type {
		case "agent_response":
			if err := s.out.Text(e.Step.Text); err != nil {
				return err
			}
			s.text.WriteString(e.Step.Text)
		case "tool":
			if e.Step.Tool != "finish" {
				return backend.Failure("POLICY_DENIED: Antigravity attempted a local action")
			}
		case "user_input", "checkpoint":
		default:
			return backend.Failure("PROTOCOL_MISMATCH: unknown Antigravity step type")
		}
	case "result":
		switch e.Result.Status {
		case "SUCCESS":
			if !s.initialized || e.Result.Session != s.session {
				return backend.Failure("PROTOCOL_MISMATCH: unexpected Antigravity result session")
			}
			if s.text.Len() == 0 {
				if err := s.out.Text(e.Result.Response); err != nil {
					return err
				}
			} else if s.text.String() != e.Result.Response {
				return backend.Failure("PROTOCOL_MISMATCH: Antigravity result conflicts with streamed text")
			}
			var usage *lmbp.Usage
			if e.Result.Usage != nil {
				usage = &lmbp.Usage{PromptTokens: e.Result.Usage.Input, CompletionTokens: e.Result.Usage.Output}
			}
			s.done = &lmbp.TaskDone{FinishReason: "stop", Usage: usage}
		case "CANCELED", "INTERRUPTED":
			return context.Canceled
		default:
			return backend.Failure("UPSTREAM_ERROR: Antigravity " + e.Result.Status + ": " + e.Result.Error)
		}
	default:
		return backend.Failure("PROTOCOL_MISMATCH: unknown Antigravity event")
	}
	return nil
}
