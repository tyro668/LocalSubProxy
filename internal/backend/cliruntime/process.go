package cliruntime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Options only contains a structured argv, never a shell command string.
type Options struct {
	Executable, Dir string
	Args, Env       []string
}

func command(ctx context.Context, o Options) *exec.Cmd {
	spec := Launch(runtime.GOOS, os.Getenv("ComSpec"), o.Executable, o.Args)
	cmd := exec.CommandContext(ctx, spec.Path)
	cmd.Args = spec.Args
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	cmd.Stderr = io.Discard // CLI diagnostics can include credentials or prompt content.
	SetProcessGroup(cmd)
	SetCmdLine(cmd, spec.CmdLine)
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error { KillProcessGroup(cmd); return nil }
	return cmd
}

// Lines drains stdout with bounded framing. The callback may write a prompt
// after a handshake; returning an error terminates and reaps the whole process.
func Lines(ctx context.Context, o Options, consume func([]byte, io.WriteCloser) error) error {
	return Exchange(ctx, o, nil, consume)
}

// Exchange sends an initial protocol request before consuming stdout. It retains
// Lines' cancellation, bounded framing, and process-group cleanup guarantees.
func Exchange(ctx context.Context, o Options, initial []byte, consume func([]byte, io.WriteCloser) error) error {
	cmd := command(ctx, o)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return err
	}
	defer in.Close()
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 64<<10), 1<<20)
	var readErr error
	if len(initial) > 0 {
		_, readErr = in.Write(initial)
	}
	for readErr == nil && scan.Scan() {
		if readErr = consume(scan.Bytes(), in); readErr != nil {
			break
		}
	}
	if readErr == nil {
		readErr = scan.Err()
	}
	if readErr != nil {
		KillProcessGroup(cmd)
	}
	waitErr := cmd.Wait()
	KillProcessGroup(cmd) // also clean up descendants retaining no stdio handles
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if readErr != nil {
		return readErr
	}
	return waitErr
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("CLI output limit exceeded")
	}
	return b.Buffer.Write(p)
}
func Output(ctx context.Context, o Options) ([]byte, error) {
	return output(ctx, o, false)
}

func output(ctx context.Context, o Options, includeStderr bool) ([]byte, error) {
	cmd := command(ctx, o)
	b := &boundedBuffer{limit: 1 << 20}
	cmd.Stdout = b
	if includeStderr {
		cmd.Stderr = b
	}
	err := cmd.Run()
	KillProcessGroup(cmd)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return b.Bytes(), err
}
func Environment(extra ...string) []string {
	return EnvironmentFor(runtime.GOOS, os.Environ(), extra...)
}
func SetEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, v := range env {
		k, _, _ := strings.Cut(v, "=")
		if !strings.EqualFold(k, key) {
			out = append(out, v)
		}
	}
	return append(out, key+"="+value)
}

// Workspace creates a private, empty repository so CLI discovery stops here.
// No user repository, instruction file, hooks, or git executable is involved.
func Workspace(prefix string) (string, error) {
	dir, err := os.MkdirTemp("", "avora-"+prefix+"-*")
	if err != nil {
		return "", err
	}
	for _, rel := range []string{".git/objects", ".git/refs/heads"} {
		if err = os.MkdirAll(filepath.Join(dir, rel), 0700); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	if err = os.WriteFile(filepath.Join(dir, ".git/HEAD"), []byte("ref: refs/heads/avora\n"), 0600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
func CheckFlags(ctx context.Context, o Options, flags ...string) error {
	o.Args = append(append([]string(nil), o.Args...), "--help")
	// Go-based CLIs commonly print --help to stderr. Only capture it for
	// capability discovery, before a prompt or credential is supplied.
	data, err := output(ctx, o, true)
	if err != nil {
		return fmt.Errorf("CLI capability check failed: %w", err)
	}
	for _, flag := range flags {
		if !strings.Contains(string(data), flag) {
			return fmt.Errorf("CLI version does not support required %s option", flag)
		}
	}
	return nil
}
