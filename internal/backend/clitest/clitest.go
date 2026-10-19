// Package clitest is the shared conformance suite for CLI backends. Each
// backend's test binary doubles as a scripted CLI: the suite hides a
// Directive in the prompt, the fake decodes it and plays the scenario through
// the backend's own wire encoding (Wire). The same expectations then hold
// for every CLI backend.
package clitest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tyro668/avora/internal/lmbp"
)

const marker = "clitest-directive:"

// Directive travels inside the prompt, so the fake needs no extra channel
// (the CLI environment is filtered and the working directory is private).
type Directive struct {
	Scenario string `json:"scenario"`
	Beat     string `json:"beat,omitempty"`   // heartbeat file kept fresh by a grandchild
	Report   string `json:"report,omitempty"` // where the fake records its environment and cwd
}

// Messages is the conversation that carries d.
func Messages(d Directive) []lmbp.Message {
	b, _ := json.Marshal(d)
	return []lmbp.Message{{Role: "user", Content: lmbp.Text(marker + string(b))}}
}

// Find extracts a directive from prompt text.
func Find(text string) (Directive, bool) {
	_, rest, ok := strings.Cut(text, marker)
	if !ok {
		return Directive{}, false
	}
	var d Directive
	dec := json.NewDecoder(strings.NewReader(rest))
	if dec.Decode(&d) != nil {
		return Directive{}, false
	}
	return d, true
}

// Wire writes abstract events in one CLI protocol. Stop reasons are the
// canonical tokens end_turn, max_tokens and cancelled.
type Wire interface {
	Manifest(w io.Writer) // the CLI's tool inventory (empty)
	Text(w io.Writer, s string)
	End(w io.Writer, stop string, in, out int)
	ToolCall(w io.Writer)
	Unknown(w io.Writer)
	Exposed(w io.Writer) // a tool inventory that is not empty
}

// Report is what the fake observed about its own process.
type Report struct {
	Env     []string `json:"env"`
	Cwd     string   `json:"cwd"`
	Entries []string `json:"entries"`
}

// Grandchild is the argv[1] that turns the test binary into a heartbeat
// grandchild; call RunGrandchild first thing in TestMain.
const Grandchild = "clitest-grandchild"

// RunGrandchild handles the grandchild role and exits.
func RunGrandchild() {
	if len(os.Args) < 3 || os.Args[1] != Grandchild {
		return
	}
	heartbeat(os.Args[2], 1000) // ends by itself after ~20s so a failed test leaks nothing
	os.Exit(0)
}

// heartbeat appends one byte per beat, so a kill mid-write never leaves the
// file empty (a truncating rewrite could, and read as "never started").
func heartbeat(path string, beats int) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(73)
	}
	defer f.Close()
	for i := 0; i < beats; i++ {
		_, _ = f.Write([]byte{'.'})
		time.Sleep(20 * time.Millisecond)
	}
}

func spawnGrandchild(beat string) {
	gc := exec.Command(os.Args[0], Grandchild, beat)
	if gc.Start() != nil {
		os.Exit(70)
	}
	for i := 0; i < 300; i++ {
		if b, err := os.ReadFile(beat); err == nil && len(b) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(71)
}

// Play performs d in the fake CLI and returns its exit code.
func Play(d Directive, w io.Writer, wire Wire) int {
	switch d.Scenario {
	case "stream":
		wire.Manifest(w)
		wire.Text(w, "hello")
		wire.Text(w, " world")
		wire.End(w, "end_turn", 6, 4)
	case "length":
		wire.Manifest(w)
		wire.Text(w, "cut")
		wire.End(w, "max_tokens", 1, 1)
	case "cancelled":
		wire.Manifest(w)
		wire.End(w, "cancelled", 0, 0)
	case "report":
		wd, _ := os.Getwd()
		var names []string
		if entries, err := os.ReadDir(wd); err == nil {
			for _, e := range entries {
				names = append(names, e.Name())
			}
		}
		b, _ := json.Marshal(Report{Env: os.Environ(), Cwd: wd, Entries: names})
		if os.WriteFile(d.Report, b, 0600) != nil {
			return 72
		}
		wire.Manifest(w)
		wire.Text(w, "reported")
		wire.End(w, "end_turn", 1, 1)
	case "hang":
		spawnGrandchild(d.Beat)
		wire.Manifest(w)
		wire.Text(w, "started")
		time.Sleep(time.Minute)
	case "after_end":
		wire.Manifest(w)
		wire.Text(w, "done")
		wire.End(w, "end_turn", 1, 1)
		wire.Text(w, "late")
	case "truncated":
		wire.Manifest(w)
		wire.Text(w, "partial")
	case "unknown":
		wire.Manifest(w)
		wire.Unknown(w)
		wire.Text(w, "x")
		wire.End(w, "end_turn", 1, 1)
	case "tool":
		spawnGrandchild(d.Beat)
		wire.Manifest(w)
		wire.ToolCall(w)
		time.Sleep(time.Minute) // a real CLI would now run the tool
	case "no_manifest":
		wire.Text(w, "x")
		wire.End(w, "end_turn", 1, 1)
	case "exposed":
		wire.Exposed(w)
		wire.Text(w, "x")
		wire.End(w, "end_turn", 1, 1)
	case "exit_failure":
		wire.Manifest(w)
		wire.Text(w, "x")
		wire.End(w, "end_turn", 1, 1)
		return 2
	default:
		fmt.Fprintln(os.Stderr, "unknown scenario", d.Scenario)
		return 73
	}
	return 0
}
