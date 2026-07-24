package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestComputeWakeDeltaSubtractsLead(t *testing.T) {
	oldLead, oldMin := wakeLead, minDelta
	defer func() { wakeLead, minDelta = oldLead, oldMin }()
	wakeLead = 15 * time.Second
	minDelta = 1

	now := time.Now()
	soonest := now.Add(120 * time.Second)
	got := computeWakeDelta(now, soonest, true)
	// 120 - 15 = 105s, ceil-rounded.
	if got != 105 {
		t.Fatalf("delta = %d, want 105 (120s due - 15s lead)", got)
	}
}

func TestComputeWakeDeltaCeilRounds(t *testing.T) {
	oldLead, oldMin := wakeLead, minDelta
	defer func() { wakeLead, minDelta = oldLead, oldMin }()
	wakeLead = 0
	minDelta = 1

	now := time.Now()
	soonest := now.Add(59700 * time.Millisecond) // 59.7s
	got := computeWakeDelta(now, soonest, true)
	if got != 60 {
		t.Fatalf("delta = %d, want 60 (ceil of 59.7s, not truncated to 59)", got)
	}
}

func TestComputeWakeDeltaRespectsMinDelta(t *testing.T) {
	oldLead, oldMin := wakeLead, minDelta
	defer func() { wakeLead, minDelta = oldLead, oldMin }()
	wakeLead = 15 * time.Second
	minDelta = 60

	now := time.Now()
	soonest := now.Add(20 * time.Second) // 20 - 15 = 5s, below minDelta
	got := computeWakeDelta(now, soonest, true)
	if got != 60 {
		t.Fatalf("delta = %d, want 60 (clamped to minDelta)", got)
	}
}

func TestComputeWakeDeltaNoJob(t *testing.T) {
	got := computeWakeDelta(time.Now(), time.Time{}, false)
	if got != maxDelta {
		t.Fatalf("delta = %d, want maxDelta %d when no job", got, maxDelta)
	}
}

// TestJobTimeoutKills verifies a job exceeding its timeout is killed and runJob
// returns promptly (does not block for the full sleep).
func TestJobTimeoutKills(t *testing.T) {
	defer withTempState(t)()
	td := t.TempDir()
	stateDir = td

	hangingJob := job{name: "hang", command: "sleep 30", timeout: 300 * time.Millisecond}
	start := time.Now()
	rc := runJob(hangingJob, nil)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("runJob blocked %s; timeout should have killed it near 300ms", elapsed)
	}
	if rc != -1 {
		t.Fatalf("rc = %d, want -1 for killed job", rc)
	}
}

// TestJobEffectiveTimeout checks per-job vs global precedence: a per-job timeout
// overrides the global default in either direction; with none set, the default
// applies.
func TestJobEffectiveTimeout(t *testing.T) {
	oldMax := jobTimeoutMax
	defer func() { jobTimeoutMax = oldMax }()
	jobTimeoutMax = 10 * time.Minute

	if got := (job{timeout: 2 * time.Minute}).effectiveTimeout(); got != 2*time.Minute {
		t.Fatalf("per-job timeout = %s, want 2m", got)
	}
	if got := (job{}).effectiveTimeout(); got != 10*time.Minute {
		t.Fatalf("default timeout = %s, want global 10m", got)
	}
	// A per-job timeout overrides the global default, even when larger.
	if got := (job{timeout: time.Hour}).effectiveTimeout(); got != time.Hour {
		t.Fatalf("per-job timeout = %s, want 1h (overrides global)", got)
	}
}

// TestReadJobParsing covers the .job key=value format: enabled variants, timeout,
// comments/blank lines, and the schedule-error cases.
func TestReadJobParsing(t *testing.T) {
	dir := t.TempDir()

	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name+".job")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	full := readJob(write("full", "# a comment\n\nschedule=every 30m\ncommand=/bin/echo hi\nenabled=0\ntimeout=5m\n"))
	if full.name != "full" {
		t.Fatalf("name = %q, want full", full.name)
	}
	if full.schedRaw != "every 30m" || full.schedErr != nil {
		t.Fatalf("schedRaw = %q err = %v", full.schedRaw, full.schedErr)
	}
	if full.command != "/bin/echo hi" {
		t.Fatalf("command = %q", full.command)
	}
	if full.enabled {
		t.Fatal("enabled=0 should parse as disabled")
	}
	if full.timeout != 5*time.Minute {
		t.Fatalf("timeout = %s, want 5m", full.timeout)
	}

	// enabled defaults to true when the key is absent.
	if got := readJob(write("noflag", "schedule=at 03:00\ncommand=x\n")); !got.enabled {
		t.Fatal("missing enabled key should default to enabled")
	}
	// Truthy spellings.
	for _, val := range []string{"1", "true", "TRUE", "yes", "Yes"} {
		got := readJob(write("t", "schedule=at 03:00\ncommand=x\nenabled="+val+"\n"))
		if !got.enabled {
			t.Fatalf("enabled=%q should be true", val)
		}
	}
	// An unparseable timeout is ignored (stays 0 = use default).
	if got := readJob(write("badto", "schedule=at 03:00\ncommand=x\ntimeout=nonsense\n")); got.timeout != 0 {
		t.Fatalf("bad timeout = %s, want 0", got.timeout)
	}
	// Missing schedule -> schedErr.
	if got := readJob(write("nosched", "command=x\n")); got.schedErr == nil {
		t.Fatal("missing schedule should set schedErr")
	}
	// Invalid schedule -> schedErr.
	if got := readJob(write("badsched", "schedule=every nonsense\ncommand=x\n")); got.schedErr == nil {
		t.Fatal("invalid schedule should set schedErr")
	}
}

// TestReadyToSuspendAbortsWhileJobRunning: with a job marked running, onReadyToSuspend
// must abort suspend (not arm the RTC), regardless of the next job's timing.
func TestReadyToSuspendAbortsWhileJobRunning(t *testing.T) {
	defer withTempState(t)()
	logp, restore := stubLipcCapture(t, true)
	defer restore()
	keepAwakeThreshold = keepAwakeDisabled // even with keep-awake off, a running job wins

	td := t.TempDir()
	jobsDir = td
	writeJob(t, td, "slow", "every 1h")
	setLastRun("slow", time.Now()) // next due far out

	dm := newDaemon()
	dm.jobDispatched() // pretend a job is running
	dm.onReadyToSuspend()

	calls := readCalls(t, logp)
	if !strings.Contains(calls, "abortSuspend") {
		t.Fatalf("expected abortSuspend while job running; calls = %q", calls)
	}
	if strings.Contains(calls, "rtcWakeup") {
		t.Fatalf("must not arm RTC while job running; calls = %q", calls)
	}
	keepAwakeThreshold = 3 * time.Minute
}

// TestReadyToSuspendArmsAfterJobDone: once no job is running and the next is far
// out, onReadyToSuspend arms the RTC normally.
func TestReadyToSuspendArmsAfterJobDone(t *testing.T) {
	defer withTempState(t)()
	logp, restore := stubLipcCapture(t, true)
	defer restore()
	keepAwakeThreshold = 3 * time.Minute

	td := t.TempDir()
	jobsDir = td
	writeJob(t, td, "slow", "every 1h")
	setLastRun("slow", time.Now())

	dm := newDaemon()
	// no job running
	dm.onReadyToSuspend()

	calls := readCalls(t, logp)
	if !strings.Contains(calls, "rtcWakeup") {
		t.Fatalf("expected RTC arm with no job running; calls = %q", calls)
	}
}

// TestDispatchDueTracksRunning verifies dispatchDue increments jobsRunning while a
// job runs and clears it after, so the running-job guard works end to end.
func TestDispatchDueTracksRunning(t *testing.T) {
	defer withTempState(t)()
	td := t.TempDir()
	jobsDir = td
	sd := t.TempDir()
	stateDir = sd

	// A job that writes a marker, sleeps briefly, due now.
	marker := filepath.Join(sd, "ran.txt")
	writeJobCmd(t, td, "slowjob", "every 1m", "sleep 0.4; echo done > "+marker)
	setLastRun("slowjob", time.Now().Add(-2*time.Minute)) // overdue -> due now

	dm := newDaemon()
	dm.dispatchDue()

	if !dm.anyJobRunning() {
		t.Fatal("jobsRunning should be >0 right after dispatch")
	}
	// Wait for completion.
	deadline := time.Now().Add(3 * time.Second)
	for dm.anyJobRunning() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if dm.anyJobRunning() {
		t.Fatal("jobsRunning never returned to 0")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("job did not run: %v", err)
	}
}

// writeJobCmd writes a .job with an explicit command, due-able via past mtime.
func writeJobCmd(t *testing.T, dir, name, sched, cmd string) {
	t.Helper()
	path := filepath.Join(dir, name+".job")
	content := "schedule=" + sched + "\ncommand=" + cmd + "\nenabled=1\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(path, old, old)
}

// TestKillRunningJobsTerminatesProcess verifies killRunningJobs actually kills a
// live job process and that the tracking map is the source of truth.
func TestKillRunningJobsTerminatesProcess(t *testing.T) {
	defer withTempState(t)()
	td := t.TempDir()
	jobsDir = td
	sd := t.TempDir()
	stateDir = sd

	writeJobCmd(t, td, "longrun", "every 1m", "sleep 30")
	setLastRun("longrun", time.Now().Add(-2*time.Minute)) // due now

	dm := newDaemon()
	dm.dispatchDue()

	// Wait until the process is actually registered (started), not just dispatched.
	deadline := time.Now().Add(2 * time.Second)
	for !dm.hasRunningProcess() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !dm.hasRunningProcess() {
		t.Fatal("job process never registered")
	}

	killed := dm.killRunningJobs()
	if killed != 1 {
		t.Fatalf("killRunningJobs returned %d, want 1", killed)
	}

	// After the kill, the goroutine's Wait returns and deregisters the job.
	deadline = time.Now().Add(3 * time.Second)
	for dm.anyJobRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if dm.anyJobRunning() {
		t.Fatal("job still registered after kill (Wait did not return)")
	}
}

// TestKillRunningJobsNoneRunning: killing with no jobs is a no-op returning 0.
func TestKillRunningJobsNoneRunning(t *testing.T) {
	dm := newDaemon()
	if killed := dm.killRunningJobs(); killed != 0 {
		t.Fatalf("killRunningJobs with none running returned %d, want 0", killed)
	}
}

// TestJobPidFilesScan verifies jobPidFiles reads name->pid and skips reserved
// kron.* and malformed files.
func TestJobPidFilesScan(t *testing.T) {
	defer withTempState(t)()
	sd := t.TempDir()
	stateDir = sd

	os.WriteFile(filepath.Join(sd, "alpha.pid"), []byte("1234\n"), 0o644)
	os.WriteFile(filepath.Join(sd, "beta.pid"), []byte("5678"), 0o644)
	os.WriteFile(filepath.Join(sd, "kron.pid"), []byte("999\n"), 0o644)  // reserved
	os.WriteFile(filepath.Join(sd, "bad.pid"), []byte("notanum"), 0o644) // malformed
	os.WriteFile(filepath.Join(sd, "alpha.last"), []byte("0"), 0o644)    // not a pid file

	got := jobPidFiles()
	if len(got) != 2 {
		t.Fatalf("got %d pid files, want 2: %v", len(got), got)
	}
	if got["alpha"] != 1234 || got["beta"] != 5678 {
		t.Fatalf("wrong pids: %v", got)
	}
	if _, ok := got["kron"]; ok {
		t.Fatal("reserved kron.pid must be excluded")
	}
	if _, ok := got["bad"]; ok {
		t.Fatal("malformed pid file must be skipped")
	}
}

// TestWriteJobPidRoundTrip verifies writeJobPid then jobPidFiles round-trips.
func TestWriteJobPidRoundTrip(t *testing.T) {
	defer withTempState(t)()
	stateDir = t.TempDir()

	writeJobPid("myjob", 4242)
	got := jobPidFiles()
	if got["myjob"] != 4242 {
		t.Fatalf("round-trip pid = %d, want 4242", got["myjob"])
	}
}
