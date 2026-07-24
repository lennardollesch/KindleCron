package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A job is one <name>.job file in jobs.d. The format (key=value) and the state
// files (state/<name>.last, Unix epoch) are the integration contract other
// applications rely on, so they are intentionally simple and stable.
type job struct {
	name     string
	schedRaw string
	sched    schedule
	schedErr error
	command  string
	enabled  bool
	mtime    time.Time     // registration anchor: when the .job file was written
	timeout  time.Duration // per-job run-time limit; 0 = use the global default
}

// jobFiles returns the paths of all .job files in jobs.d, sorted by name so the
// evaluation order is stable from run to run.
func jobFiles() []string {
	matches, _ := filepath.Glob(filepath.Join(jobsDir, "*.job"))
	sort.Strings(matches)
	return matches
}

// readJob parses one .job file. It never fails: a file that cannot be read or
// carries no usable schedule comes back with schedErr set, which makes the
// daemon skip it and `kron list` mark it INVALID.
func readJob(path string) job {
	parsed := job{name: strings.TrimSuffix(filepath.Base(path), ".job"), enabled: true}
	file, err := os.Open(path)
	if err != nil {
		parsed.schedErr = err
		return parsed
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil {
		parsed.mtime = info.ModTime()
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimRight(scanner.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		equalIndex := strings.IndexByte(line, '=')
		if equalIndex < 0 {
			continue
		}
		key := strings.TrimSpace(line[:equalIndex])
		val := strings.TrimSpace(line[equalIndex+1:])
		switch key {
		case "schedule":
			parsed.schedRaw = val
		case "command":
			parsed.command = val
		case "enabled":
			parsed.enabled = val == "1" || strings.EqualFold(val, "true") || strings.EqualFold(val, "yes")
		case "timeout":
			if duration, err := time.ParseDuration(val); err == nil && duration > 0 {
				parsed.timeout = duration
			}
		}
	}
	if parsed.schedRaw == "" {
		parsed.schedErr = fmt.Errorf("no schedule")
	} else {
		parsed.sched, parsed.schedErr = parseSchedule(parsed.schedRaw)
	}
	return parsed
}

// getLastRun reads a job's last-run stamp from state/<name>.last. A missing or
// unreadable stamp reads as the Unix epoch, which the schedule forms treat as
// "never run".
func getLastRun(name string) time.Time {
	data, err := os.ReadFile(lastPath(name))
	if err != nil {
		return time.Unix(0, 0)
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Unix(0, 0)
	}
	return time.Unix(seconds, 0)
}

// setLastRun records a job's last-run stamp. A write failure is logged rather
// than returned: the job itself already ran, and the next evaluation simply
// re-reads whatever stamp is on disk.
func setLastRun(name string, stamp time.Time) {
	if err := atomicWrite(lastPath(name), []byte(strconv.FormatInt(stamp.Unix(), 10)+"\n")); err != nil {
		logf("set last-run %s failed: %v", name, err)
	}
}

// atomicWrite replaces the file at path with data, so a reader never observes a
// half-written state file. The data goes to a temporary file in the same
// directory (a rename is only atomic within one filesystem), is flushed to
// storage, and only then replaces the target. If the device loses power at any
// point, path still holds either the complete old or the complete new content.
func atomicWrite(path string, data []byte) error {
	tempFile, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	tempFile.Chmod(0o644) // CreateTemp makes 0600; the state files are expected at 0644
	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close()
		os.Remove(tempPath)
		return err
	}
	if err := tempFile.Sync(); err != nil { // flush to storage before the rename
		tempFile.Close()
		os.Remove(tempPath)
		return err
	}
	if err := tempFile.Close(); err != nil {
		os.Remove(tempPath)
		return err
	}
	return os.Rename(tempPath, path)
}

// jobTimeoutMax is the default run-time limit for a job that does not set its own
// timeout=. A job that exceeds its effective limit (per-job timeout= or this
// default) is killed, so a hung job can never pin the device at ready-to-suspend.
// Settable via -jobtimeout; a per-job timeout= overrides it in either direction.
var jobTimeoutMax = 10 * time.Minute

// effectiveTimeout is the per-job timeout if set, otherwise the global default.
func (entry job) effectiveTimeout() time.Duration {
	if entry.timeout > 0 {
		return entry.timeout
	}
	return jobTimeoutMax
}

// runJob executes a job, enforcing its timeout. If register is non-nil it is
// called with the command just before it starts and must return a deregister
// func, run when the command finishes; this lets the daemon track the live
// process so it can terminate it on shutdown. register may be nil (CLI path).
func runJob(entry job, register func(*exec.Cmd) func()) int {
	logf("run '%s': %s", entry.name, entry.command)
	timeout := entry.effectiveTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", entry.command)
	// Run in its own process group and, on timeout (ctx cancel), kill the WHOLE
	// group rather than just the sh process. Otherwise the actual command (a
	// grandchild) is orphaned and keeps running past the timeout. This also makes
	// the tracked process killable as a group on shutdown.
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}
	if file, err := os.Create(logPath(entry.name)); err == nil {
		cmd.Stdout, cmd.Stderr = file, file
		defer file.Close()
	}
	if register != nil {
		// Start explicitly so the process exists before register, then wait.
		if err := cmd.Start(); err != nil {
			logf("done '%s' (start failed: %v)", entry.name, err)
			return -1
		}
		deregister := register(cmd)
		err := cmd.Wait()
		deregister()
		return jobExitCode(ctx, entry, timeout, err)
	}
	err := cmd.Run()
	return jobExitCode(ctx, entry, timeout, err)
}

// writeJobPid records a running job's process-group id (the leader pid) so an
// external `kron kill-jobs` can terminate it after an unclean daemon death. Removed
// when the job finishes; a leftover file means the daemon died without cleanup.
func writeJobPid(name string, pid int) {
	atomicWrite(jobPidPath(name), []byte(strconv.Itoa(pid)+"\n"))
}

// jobExitCode logs how a finished job ended and reports its exit code. A job
// that could not be started, or that was killed on timeout, reports -1.
func jobExitCode(ctx context.Context, entry job, timeout time.Duration, err error) int {
	returnCode := 0
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			logf("TIMEOUT '%s' after %s - killed (device can suspend again)", entry.name, timeout)
			return -1
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			returnCode = exitErr.ExitCode()
		} else {
			returnCode = -1
		}
	}
	logf("done '%s' (exit %d)", entry.name, returnCode)
	return returnCode
}
