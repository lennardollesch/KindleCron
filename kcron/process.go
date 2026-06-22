package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// acquireSingleton takes an exclusive, non-blocking advisory lock on path. The
// returned file must stay open to hold the lock; closing it (or process exit)
// releases it. ok=false means another instance already holds it.
func acquireSingleton(path string) (*os.File, bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	f.Truncate(0)
	f.Seek(0, 0)
	f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	f.Sync()
	return f, true
}

func shutdownSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}

// signalStop asks the daemon with the given PID to shut down gracefully.
func signalStop(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// setProcessGroup makes the command start in its own process group, so the whole
// job (the sh -c and any children it spawns) can be killed together. Without this,
// killing only the direct child (sh) orphans grandchildren (e.g. the actual
// command), which keep running after the daemon exits.
func setProcessGroup(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
}

// killProcessGroupByPID kills the process group led by pid (negative-pid kill).
// Used by `kcron kill-jobs` to terminate orphaned job trees recorded in pid files,
// where no *exec.Cmd is available. Returns true if a signal was delivered.
func killProcessGroupByPID(pid int) bool {
	if pid <= 1 {
		return false // never signal pid 0/1 or process-group 'all'
	}
	return syscall.Kill(-pid, syscall.SIGKILL) == nil
}

// processGroupAlive reports whether the process group led by pid still has any
// member (signal 0 probes without killing).
func processGroupAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	return syscall.Kill(-pid, 0) == nil
}

// killProcessGroup terminates the entire process group of a started command.
// Killing the negative PID (the process-group id, which equals the leader's PID
// because we set Setpgid) signals every process in the group. Returns true if a
// signal was delivered.
func killProcessGroup(c *exec.Cmd) bool {
	if c.Process == nil {
		return false
	}
	pgid := c.Process.Pid // leader pid == pgid due to Setpgid
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		// Fall back to killing just the process if the group kill failed.
		return c.Process.Kill() == nil
	}
	return true
}
