package cliruntime

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

type tree struct {
	mu  sync.Mutex
	job windows.Handle
	err error
}

func (t *tree) prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.err = err
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		t.err = err
		return
	}
	t.job = job
}

// attach puts the suspended process into the job before any of its code
// (and so any child) runs, then resumes it.
func (t *tree) attach(cmd *exec.Cmd) error {
	if t.err != nil {
		return t.err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err = windows.AssignProcessToJobObject(t.job, h); err != nil {
		return err
	}
	if status, _, _ := ntResumeProcess.Call(uintptr(h)); status != 0 {
		return errors.New("NtResumeProcess failed")
	}
	return nil
}

func (t *tree) kill(*exec.Cmd) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
	}
}

// awaitExit has nothing to do: the job handle (not a PID) addresses the
// tree, so ending it after Wait is safe.
func (t *tree) awaitExit(*exec.Cmd, time.Duration) bool { return true }

// killDescendants is left to release, which runs after Wait.
func (t *tree) killDescendants(*exec.Cmd) {}

// release ends whatever is left of the tree and closes the job.
func (t *tree) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
		_ = windows.CloseHandle(t.job)
		t.job = 0
	}
}
