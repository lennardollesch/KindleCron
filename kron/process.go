package main

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// ignoreSIGPIPE keeps a write to a closed stdout/stderr from killing the process.
// KUAL launches menu actions with their stdout connected to a pipe it does not read
// and then closes; by default the first write to it delivers SIGPIPE, whose default
// action on fd 1/2 is to terminate the process. Every command that prints its result
// before drawing it (version, purge, stop, ...) would therefore die just short of the
// eips draw. Ignoring SIGPIPE turns those writes into harmless EPIPE errors, so the
// draw still runs and KUAL users see an outcome.
func ignoreSIGPIPE() {
	signal.Ignore(syscall.SIGPIPE)
}

// rootReadonly reports whether the root filesystem is currently mounted
// read-only. ok is false when the state could not be determined (e.g. /proc not
// mounted), in which case the caller should just attempt its write.
func rootReadonly() (readOnly bool, ok bool) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, false
	}
	return parseRootReadonly(string(data))
}

// parseRootReadonly extracts the read-only/read-write state of the "/" mount from
// the contents of /proc/mounts. Split out from rootReadonly so it can be tested.
// When "/" is mounted more than once (e.g. an initramfs 'rootfs' shadowed by the
// real device), the last entry is the effective mount, so later entries win.
func parseRootReadonly(mounts string) (readOnly bool, ok bool) {
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "/" {
			continue
		}
		for _, option := range strings.Split(fields[3], ",") {
			switch option {
			case "ro":
				readOnly, ok = true, true
			case "rw":
				readOnly, ok = false, true
			}
		}
	}
	return readOnly, ok
}

// remountRoot switches the root filesystem between read-write (writable=true) and
// read-only. It prefers the Kindle 'mntroot' helper, which knows the device's
// partition specifics, and falls back to a plain remount. Requires root, which
// KUAL menu actions and a manual `sudo`/root shell both provide.
func remountRoot(writable bool) error {
	mode := "ro"
	if writable {
		mode = "rw"
	}
	if mntroot := findMntroot(); mntroot != "" {
		if err := exec.Command(mntroot, mode).Run(); err == nil {
			return nil
		}
	}
	return exec.Command("mount", "-o", "remount,"+mode, "/").Run()
}

// syncDisk flushes buffered filesystem writes to storage. Called after writing to
// the briefly-writable root so the change is on the eMMC before it is remounted
// read-only, matching the Kindle 'mntroot rw ... mntroot ro' procedure.
func syncDisk() {
	syscall.Sync()
}

// findMntroot locates the Kindle 'mntroot' helper, returning "" if it is absent.
func findMntroot() string {
	if path, err := exec.LookPath("mntroot"); err == nil {
		return path
	}
	const fixed = "/usr/sbin/mntroot"
	if _, err := os.Stat(fixed); err == nil {
		return fixed
	}
	return ""
}

// acquireSingleton takes an exclusive, non-blocking advisory lock on path. The
// returned file must stay open to hold the lock; closing it (or process exit)
// releases it. ok=false means another instance already holds it.
func acquireSingleton(path string) (*os.File, bool) {
	lockFile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		return nil, false
	}
	// Record the holder's pid in the file so `kron stop` knows whom to signal.
	lockFile.Truncate(0)
	lockFile.Seek(0, 0)
	lockFile.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	lockFile.Sync()
	return lockFile, true
}

// shutdownSignals lists the signals that make the daemon shut down cleanly.
func shutdownSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM}
}

// signalStop asks the daemon with the given PID to shut down gracefully.
func signalStop(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// lockHeld reports whether a process currently holds the exclusive singleton lock
// on path (i.e. the daemon is running). It opens the file and tries a non-blocking
// exclusive lock: success means no one holds it (the lock is released again and the
// file left untouched), failure means a daemon holds it. `kron stop` uses this to
// avoid signalling a recycled pid from a stale lock file left by a crashed daemon.
func lockHeld(path string) bool {
	lockFile, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return false // no lock file -> no daemon
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true // someone holds it
	}
	syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	return false
}

// setProcessGroup makes the command start in its own process group, so the whole
// job (the sh -c and any children it spawns) can be killed together. Without this,
// killing only the direct child (sh) orphans grandchildren (e.g. the actual
// command), which keep running after the daemon exits.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroupByPID kills the process group led by pid (negative-pid kill).
// Used by `kron kill-jobs` to terminate orphaned job trees recorded in pid files,
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
// because setProcessGroup set Setpgid) signals every process in the group.
// Returns true if a signal was delivered.
func killProcessGroup(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	groupID := cmd.Process.Pid // leader pid == pgid due to Setpgid
	if err := syscall.Kill(-groupID, syscall.SIGKILL); err != nil {
		// Fall back to killing just the process if the group kill failed.
		return cmd.Process.Kill() == nil
	}
	return true
}
