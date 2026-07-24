package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTempJobs points jobsDir at a fresh temp directory and restores it after.
func withTempJobs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := jobsDir
	jobsDir = dir
	t.Cleanup(func() { jobsDir = old })
	return dir
}

func TestSetJobEnabledTogglesAndPreservesKeys(t *testing.T) {
	withTempJobs(t)
	name := "demo"
	body := "schedule=every 30m\ncommand=/bin/echo hi\nenabled=1\ntimeout=5m\n"
	if err := os.WriteFile(jobPath(name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := setJobEnabled(name, false); err != nil {
		t.Fatal(err)
	}
	disabled := readJob(jobPath(name))
	if disabled.enabled {
		t.Fatal("job should be disabled")
	}
	if disabled.schedRaw != "every 30m" || disabled.command != "/bin/echo hi" || disabled.timeout != 5*time.Minute {
		t.Fatalf("other keys not preserved: %+v", disabled)
	}

	if err := setJobEnabled(name, true); err != nil {
		t.Fatal(err)
	}
	if !readJob(jobPath(name)).enabled {
		t.Fatal("job should be enabled again")
	}
}

func TestSetJobEnabledAppendsWhenKeyMissing(t *testing.T) {
	withTempJobs(t)
	name := "noflag"
	if err := os.WriteFile(jobPath(name), []byte("schedule=at 03:00\ncommand=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setJobEnabled(name, false); err != nil {
		t.Fatal(err)
	}
	got := readJob(jobPath(name))
	if got.enabled {
		t.Fatal("job should be disabled after appending enabled=0")
	}
	if got.schedRaw != "at 03:00" || got.command != "x" {
		t.Fatalf("keys lost when appending: %+v", got)
	}
}

func TestSetJobEnabledMissingJob(t *testing.T) {
	withTempJobs(t)
	err := setJobEnabled("ghost", true)
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("setJobEnabled on missing job = %v, want not-exist error", err)
	}
}

func TestJobListLine(t *testing.T) {
	defer withTempState(t)() // stateDir for getLastRun
	oldThreshold := keepAwakeThreshold
	keepAwakeThreshold = 3 * time.Minute
	defer func() { keepAwakeThreshold = oldThreshold }()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)

	// Short interval under the threshold -> flagged keep-awake, shows next-due.
	short := job{name: "quick", schedRaw: "every 1m", sched: schedule{kind: kindEvery, interval: time.Minute}, enabled: true, mtime: now}
	line, isShort := jobListLine(short, now)
	if !isShort {
		t.Fatal("every-1m under threshold should be a keep-awake job")
	}
	if !strings.Contains(line, "[keep awake]") || !strings.Contains(line, "quick") || !strings.Contains(line, "next:") {
		t.Fatalf("short-job line = %q", line)
	}

	// Short-cadence cron (every 30s) is flagged keep-awake too.
	cronSched, err := parseSchedule("*/30 * * * * *")
	if err != nil {
		t.Fatal(err)
	}
	cronJob := job{name: "poll", schedRaw: "*/30 * * * * *", sched: cronSched, enabled: true, mtime: now}
	line, isShort = jobListLine(cronJob, now)
	if !isShort || !strings.Contains(line, "[keep awake]") {
		t.Fatalf("short-cadence cron should be keep-awake: %q", line)
	}

	// Per-job timeout is shown; a long interval is not keep-awake.
	withTimeout := job{name: "slow", schedRaw: "every 2h", sched: schedule{kind: kindEvery, interval: 2 * time.Hour}, enabled: true, mtime: now, timeout: 5 * time.Minute}
	line, isShort = jobListLine(withTimeout, now)
	if isShort {
		t.Fatal("a 2h interval is not a keep-awake job")
	}
	if !strings.Contains(line, "timeout 5m0s") {
		t.Fatalf("per-job timeout not shown: %q", line)
	}

	// Disabled job: status "disabled", never counts as keep-awake.
	disabled := job{name: "off", schedRaw: "every 1m", sched: schedule{kind: kindEvery, interval: time.Minute}, enabled: false, mtime: now}
	line, isShort = jobListLine(disabled, now)
	if isShort {
		t.Fatal("a disabled job must not be flagged keep-awake")
	}
	if !strings.Contains(line, "disabled") {
		t.Fatalf("disabled status missing: %q", line)
	}

	// Invalid schedule: status "INVALID:".
	invalid := job{name: "bad", schedRaw: "every nope", schedErr: fmt.Errorf("bad interval"), enabled: true, mtime: now}
	line, _ = jobListLine(invalid, now)
	if !strings.Contains(line, "INVALID:") {
		t.Fatalf("invalid status missing: %q", line)
	}
}

func TestValidateJobName(t *testing.T) {
	valid := []string{"backup", "job-1", "My_Job.2", "with space"}
	for _, name := range valid {
		if err := validateJobName(name); err != nil {
			t.Errorf("validateJobName(%q) = %v, want nil", name, err)
		}
	}
	// Empty, path traversal, hidden/dot names, and the reserved daemon name.
	invalid := []string{"", "../escape", "a/b", `a\b`, ".", "..", ".hidden", "kron"}
	for _, name := range invalid {
		if err := validateJobName(name); err == nil {
			t.Errorf("validateJobName(%q) = nil, want an error", name)
		}
	}
}

func TestDirOnPath(t *testing.T) {
	dir := filepath.Dir(kronLinkPath)
	t.Setenv("PATH", strings.Join([]string{"/sbin", dir, "/bin"}, string(os.PathListSeparator)))
	if !dirOnPath(dir) {
		t.Fatalf("%s not found on PATH", dir)
	}
	if dirOnPath("/nonexistent/xyz") {
		t.Fatal("reported /nonexistent/xyz on PATH")
	}
}
