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

func jobFiles() []string {
	matches, _ := filepath.Glob(filepath.Join(jobsDir, "*.job"))
	sort.Strings(matches)
	return matches
}

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

func setLastRun(name string, t time.Time) {
	if err := atomicWrite(lastPath(name), []byte(strconv.FormatInt(t.Unix(), 10)+"\n")); err != nil {
		logf("set last-run %s failed: %v", name, err)
	}
}

// atomicWrite writes via a temp file in the same directory, then renames.
func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// jobTimeoutMax is the default run-time limit for a job that does not set its own
// timeout=. A job that exceeds its effective limit (per-job timeout= or this
// default) is killed, so a hung job can never pin the device at ready-to-suspend.
// Settable via -jobtimeout; a per-job timeout= overrides it in either direction.
var jobTimeoutMax = 10 * time.Minute

// effectiveTimeout is the per-job timeout if set, otherwise the global default.
func (def job) effectiveTimeout() time.Duration {
	if def.timeout > 0 {
		return def.timeout
	}
	return jobTimeoutMax
}

// runJob executes a job, enforcing its timeout. If register is non-nil it is
// called with the command just before it starts and must return a deregister
// func, run when the command finishes; this lets the daemon track the live
// process so it can terminate it on shutdown. register may be nil (CLI path).
func runJob(def job, register func(*exec.Cmd) func()) int {
	logf("run '%s': %s", def.name, def.command)
	timeout := def.effectiveTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", def.command)
	// Run in its own process group and, on timeout (ctx cancel), kill the WHOLE
	// group rather than just the sh process. Otherwise the actual command (a
	// grandchild) is orphaned and keeps running past the timeout. This also makes
	// the tracked process killable as a group on shutdown.
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}
	if file, err := os.Create(logPath(def.name)); err == nil {
		cmd.Stdout, cmd.Stderr = file, file
		defer file.Close()
	}
	if register != nil {
		// Start explicitly so the process exists before we register it, then wait.
		if err := cmd.Start(); err != nil {
			logf("done '%s' (start failed: %v)", def.name, err)
			return -1
		}
		deregister := register(cmd)
		err := cmd.Wait()
		deregister()
		return jobExitCode(ctx, def, timeout, err)
	}
	err := cmd.Run()
	return jobExitCode(ctx, def, timeout, err)
}

// writeJobPid records a running job's process-group id (the leader pid) so an
// external `kcron kill-jobs` can terminate it after an unclean daemon death. Removed
// when the job finishes; a leftover file means the daemon died without cleanup.
func writeJobPid(name string, pid int) {
	atomicWrite(jobPidPath(name), []byte(strconv.Itoa(pid)+"\n"))
}

func jobExitCode(ctx context.Context, def job, timeout time.Duration, err error) int {
	returnCode := 0
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			logf("TIMEOUT '%s' after %s - killed (device can suspend again)", def.name, timeout)
			return -1
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			returnCode = exitErr.ExitCode()
		} else {
			returnCode = -1
		}
	}
	logf("done '%s' (exit %d)", def.name, returnCode)
	return returnCode
}
