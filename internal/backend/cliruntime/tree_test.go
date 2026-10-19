package cliruntime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a scripted CLI: AVORA_TREE_ROLE=child starts a
// grandchild that keeps a heartbeat file fresh, then exits or hangs.
func TestMain(m *testing.M) {
	switch os.Getenv("AVORA_TREE_ROLE") {
	case "child":
		treeChild()
		return
	case "grandchild":
		treeGrandchild()
		return
	case "help":
		f, _ := os.OpenFile(os.Getenv("AVORA_TREE_BEAT"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		f.WriteString("spawn\n")
		f.Close()
		fmt.Println("--agent --sandbox")
		os.Exit(0)
	case "slow":
		time.Sleep(exitGrace + time.Second)
		fmt.Println("slow answer")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func treeChild() {
	gc := exec.Command(os.Args[0])
	gc.Env = append(os.Environ(), "AVORA_TREE_ROLE=grandchild")
	if err := gc.Start(); err != nil {
		os.Exit(3)
	}
	beat := os.Getenv("AVORA_TREE_BEAT")
	for i := 0; i < 200; i++ { // the grandchild is running before the child reports
		if b, err := os.ReadFile(beat); err == nil && len(b) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Println("ready")
	if os.Getenv("AVORA_TREE_MODE") == "hang" {
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func treeGrandchild() {
	beat := os.Getenv("AVORA_TREE_BEAT")
	_ = os.WriteFile(beat+".pid", []byte(strconv.Itoa(os.Getpid())), 0600)
	for i := 1; i < 1000; i++ { // ends by itself after ~20s so a failed test leaks nothing
		_ = os.WriteFile(beat, []byte(strconv.Itoa(i)), 0600)
		time.Sleep(20 * time.Millisecond)
	}
}

func treeOptions(t *testing.T, mode string) (Options, string) {
	beat := filepath.Join(t.TempDir(), "beat")
	env := append(os.Environ(), "AVORA_TREE_ROLE=child", "AVORA_TREE_MODE="+mode, "AVORA_TREE_BEAT="+beat)
	return Options{Executable: os.Args[0], Dir: t.TempDir(), Env: env}, beat
}

// alive reports whether the heartbeat file still changes.
func alive(beat string) bool {
	read := func() []byte {
		for i := 0; i < 20; i++ {
			if b, err := os.ReadFile(beat); err == nil && len(b) > 0 {
				return b
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}
	first := read()
	for i := 0; i < 10; i++ {
		time.Sleep(60 * time.Millisecond)
		if !bytes.Equal(first, read()) {
			return true
		}
	}
	return false
}

func requireTreeGone(t *testing.T, beat string) {
	t.Helper()
	if _, err := os.Stat(beat); err != nil {
		t.Fatalf("grandchild never started: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if alive(beat) {
		t.Fatal("grandchild survived its CLI process")
	}
}

func TestTreeEndsGrandchildAfterNormalExit(t *testing.T) {
	o, beat := treeOptions(t, "exit")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lines := 0
	if err := Lines(ctx, o, func([]byte, io.WriteCloser) error { lines++; return nil }); err != nil || lines != 1 {
		t.Fatalf("lines=%d err=%v", lines, err)
	}
	requireTreeGone(t, beat)
}

func TestTreeEndsGrandchildOnCancel(t *testing.T) {
	o, beat := treeOptions(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := Lines(ctx, o, func([]byte, io.WriteCloser) error { cancel(); return nil })
	if err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
	requireTreeGone(t, beat)
}

func TestTreeEndsGrandchildOnConsumerError(t *testing.T) {
	o, beat := treeOptions(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stop := fmt.Errorf("stop")
	start := time.Now()
	if err := Lines(ctx, o, func([]byte, io.WriteCloser) error { return stop }); err != stop {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("consumer error did not end the process promptly")
	}
	requireTreeGone(t, beat)
}

func TestTreeEndsGrandchildAfterOutput(t *testing.T) {
	o, beat := treeOptions(t, "exit")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := Output(ctx, o); err != nil || string(bytes.TrimSpace(out)) != "ready" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	requireTreeGone(t, beat)
}

// Output has no end-of-output signal: a CLI slower than exitGrace (agy
// models takes seconds) must not be killed.
func TestOutputWaitsForSlowCLI(t *testing.T) {
	env := append(os.Environ(), "AVORA_TREE_ROLE=slow")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b, err := Output(ctx, Options{Executable: os.Args[0], Env: env, Dir: t.TempDir()})
	if err != nil || strings.TrimSpace(string(b)) != "slow answer" {
		t.Fatalf("%q %v", b, err)
	}
}

// CheckFlags runs --help once per executable version, not per task.
func TestCheckFlagsCachedByPathAndMtime(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "fakecli")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	self, err := os.ReadFile(os.Args[0])
	if err != nil || os.WriteFile(exe, self, 0700) != nil {
		t.Fatal(err)
	}
	count := filepath.Join(dir, "spawns")
	o := Options{Executable: exe, Dir: dir, Env: append(os.Environ(), "AVORA_TREE_ROLE=help", "AVORA_TREE_BEAT="+count)}
	spawns := func() int { b, _ := os.ReadFile(count); return bytes.Count(b, []byte("spawn")) }
	for i := 0; i < 3; i++ {
		if err := CheckFlags(context.Background(), o, "--agent"); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckFlags(context.Background(), o, "--missing"); err == nil {
		t.Fatal("missing flag accepted")
	}
	if n := spawns(); n != 1 {
		t.Fatalf("%d --help runs, want 1", n)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(exe, later, later); err != nil { // an upgraded CLI
		t.Fatal(err)
	}
	if err := CheckFlags(context.Background(), o, "--sandbox"); err != nil || spawns() != 2 {
		t.Fatalf("upgrade not re-checked: %v %d", err, spawns())
	}
}
