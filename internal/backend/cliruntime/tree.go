package cliruntime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

// exitGrace bounds how long a CLI may keep running after it closed stdout.
const exitGrace = 3 * time.Second

// Process is one CLI process together with every descendant it starts.
//
// Unix: the CLI leads its own process group. The group is signalled only
// while the leader is still unreaped (running or a zombie), so its group ID
// cannot have been reused by an unrelated process.
// Windows: the CLI starts suspended, joins a Job Object with
// KILL_ON_JOB_CLOSE, then resumes; ending the job ends the whole tree,
// including orphaned grandchildren, and never addresses a bare PID.
type Process struct {
	Cmd     *exec.Cmd
	mu      sync.Mutex
	reaping bool
	tree    tree
}

// Command builds the process for o. The caller may set pipes, then Start.
func Command(ctx context.Context, o Options) *Process {
	spec := Launch(runtime.GOOS, os.Getenv("ComSpec"), o.Executable, o.Args)
	cmd := exec.CommandContext(ctx, spec.Path)
	cmd.Args = spec.Args
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	cmd.Stderr = io.Discard // CLI diagnostics can include credentials or prompt content.
	SetCmdLine(cmd, spec.CmdLine)
	p := &Process{Cmd: cmd}
	p.tree.prepare(cmd)
	cmd.WaitDelay = exitGrace
	cmd.Cancel = func() error { p.Kill(); return nil }
	return p
}

// Start starts the process inside its tree; a process that cannot be
// isolated is terminated and reported as an error.
func (p *Process) Start() error {
	if err := p.Cmd.Start(); err != nil {
		p.tree.release()
		return err
	}
	if err := p.tree.attach(p.Cmd); err != nil {
		_ = p.Cmd.Process.Kill()
		_ = p.Cmd.Wait()
		p.tree.release()
		return fmt.Errorf("CLI process isolation failed: %w", err)
	}
	return nil
}

// Kill ends the process and all of its descendants. Once Wait has begun
// reaping the process it does nothing.
func (p *Process) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.reaping {
		p.tree.kill(p.Cmd)
	}
}

// Wait waits for the process to exit (or its context to end it), ends every
// descendant still running, and reaps it.
func (p *Process) Wait() error {
	for !p.tree.awaitExit(p.Cmd, time.Hour) {
	}
	return p.reap()
}

// Finish is Wait for a CLI whose output has ended: one that keeps running
// exitGrace later is killed.
func (p *Process) Finish() error { return p.wait(exitGrace) }

func (p *Process) wait(grace time.Duration) error {
	if !p.tree.awaitExit(p.Cmd, grace) {
		p.Kill()
		p.tree.awaitExit(p.Cmd, exitGrace)
	}
	return p.reap()
}

// reap ends the descendants of the exited, unreaped leader, then reaps it.
func (p *Process) reap() error {
	p.mu.Lock()
	p.tree.killDescendants(p.Cmd) // the leader has exited but is not reaped yet
	p.reaping = true
	p.mu.Unlock()
	err := p.Cmd.Wait()
	p.tree.release()
	return err
}
