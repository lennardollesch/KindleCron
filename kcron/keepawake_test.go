package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withTempState(t *testing.T) func() {
	t.Helper()
	d := t.TempDir()
	oldState := stateDir
	stateDir = d
	return func() { stateDir = oldState }
}

// TestActiveThresholdFixed verifies the fixed threshold and the off sentinel.
func TestActiveThresholdFixed(t *testing.T) {
	old := keepAwakeThreshold
	defer func() { keepAwakeThreshold = old }()

	keepAwakeThreshold = 3 * time.Minute
	if got := activeThreshold(); got != 3*time.Minute {
		t.Fatalf("activeThreshold = %s, want 3m", got)
	}

	keepAwakeThreshold = 90 * time.Second
	if got := activeThreshold(); got != 90*time.Second {
		t.Fatalf("activeThreshold = %s, want 90s", got)
	}

	keepAwakeThreshold = keepAwakeDisabled
	if got := activeThreshold(); got != 0 {
		t.Fatalf("activeThreshold with off = %s, want 0", got)
	}
}

// stubLipcCapture replaces lipc-set-prop with a script that records its args to a
// file, so tests can assert what property/value the daemon set. succeed controls
// the exit code (a failing stub mimics powerd rejecting abortSuspend outside the
// readyToSuspend window).
func stubLipcCapture(t *testing.T, succeed bool) (logPath string, restore func()) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	exit := "0"
	if !succeed {
		exit = "1"
	}
	script := "#!/bin/sh\necho \"$*\" >> " + logPath + "\nexit " + exit + "\n"
	p := filepath.Join(dir, "lipc-set-prop")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Skipf("cannot create lipc stub: %v", err)
	}
	old := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+old)
	return logPath, func() { os.Setenv("PATH", old) }
}

func readCalls(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// TestAbortSuspendValue verifies requestAbortSuspend writes the abortSuspend
// trigger with value 1.
func TestAbortSuspendValue(t *testing.T) {
	defer withTempState(t)()
	logp, restore := stubLipcCapture(t, true)
	defer restore()

	if err := requestAbortSuspend(); err != nil {
		t.Fatalf("requestAbortSuspend: %v", err)
	}
	calls := readCalls(t, logp)
	if !strings.Contains(calls, "abortSuspend") {
		t.Fatalf("did not set abortSuspend; calls = %q", calls)
	}
	if !strings.Contains(calls, " 1") {
		t.Fatalf("abortSuspend trigger value not 1; calls = %q", calls)
	}
}

// TestAbortSuspendRejected mimics powerd rejecting the property (wrong state):
// requestAbortSuspend must return an error so the caller falls back to suspend.
func TestAbortSuspendRejected(t *testing.T) {
	defer withTempState(t)()
	_, restore := stubLipcCapture(t, false)
	defer restore()

	if err := requestAbortSuspend(); err == nil {
		t.Fatal("expected error when powerd rejects abortSuspend")
	}
}

// TestReadyToSuspendDefersShortJob: with a job due within the threshold, the
// daemon must call abortSuspend and NOT arm the RTC.
func TestReadyToSuspendDefersShortJob(t *testing.T) {
	defer withTempState(t)()
	logp, restore := stubLipcCapture(t, true)
	defer restore()
	keepAwakeThreshold = 3 * time.Minute

	// Set up jobsDir with a job due ~30s out (under 5m threshold).
	td := t.TempDir()
	jobsDir = td
	writeJob(t, td, "quick", "every 1m")

	d := newDaemon()
	d.onReadyToSuspend()

	calls := readCalls(t, logp)
	if !strings.Contains(calls, "abortSuspend") {
		t.Fatalf("expected abortSuspend for imminent job; calls = %q", calls)
	}
	if strings.Contains(calls, "rtcWakeup") {
		t.Fatalf("must not arm RTC when deferring; calls = %q", calls)
	}
	if !d.deferring {
		t.Fatal("daemon did not record deferring state")
	}
}

// TestReadyToSuspendAbortsEveryReadyEvent: an imminent job must trigger an abort on
// every readyToSuspend, not only the first, so the device keeps deferring suspend
// for as long as the job stays imminent.
func TestReadyToSuspendAbortsEveryReadyEvent(t *testing.T) {
	defer withTempState(t)()
	logp, restore := stubLipcCapture(t, true)
	defer restore()
	oldThreshold := keepAwakeThreshold
	keepAwakeThreshold = 3 * time.Minute
	defer func() { keepAwakeThreshold = oldThreshold }()

	td := t.TempDir()
	jobsDir = td
	writeJob(t, td, "quick", "every 1m")

	d := newDaemon()
	d.onReadyToSuspend()
	d.onReadyToSuspend()

	if got := strings.Count(readCalls(t, logp), "abortSuspend"); got != 2 {
		t.Fatalf("abortSuspend calls = %d, want 2 (one per readyToSuspend)", got)
	}
}

// TestReadyToSuspendArmsLongJob: with the next job far out, the daemon must arm
// the RTC and NOT defer.
func TestReadyToSuspendArmsLongJob(t *testing.T) {
	defer withTempState(t)()
	logp, restore := stubLipcCapture(t, true)
	defer restore()
	keepAwakeThreshold = 3 * time.Minute

	td := t.TempDir()
	jobsDir = td
	writeJob(t, td, "slow", "every 1h")
	// Mark it as just-run so the next due time is ~1h out, not overdue.
	setLastRun("slow", time.Now())

	d := newDaemon()
	d.onReadyToSuspend()

	calls := readCalls(t, logp)
	if !strings.Contains(calls, "rtcWakeup") {
		t.Fatalf("expected RTC arm for distant job; calls = %q", calls)
	}
	if strings.Contains(calls, "abortSuspend") {
		t.Fatalf("must not abort for distant job; calls = %q", calls)
	}
}

// TestDeferDisabledArmsRTC: with keep-awake off, even a short job arms the RTC.
func TestDeferDisabledArmsRTC(t *testing.T) {
	defer withTempState(t)()
	logp, restore := stubLipcCapture(t, true)
	defer restore()
	keepAwakeThreshold = keepAwakeDisabled

	td := t.TempDir()
	jobsDir = td
	writeJob(t, td, "quick", "every 1m")

	d := newDaemon()
	d.onReadyToSuspend()

	calls := readCalls(t, logp)
	if strings.Contains(calls, "abortSuspend") {
		t.Fatalf("keep-awake off must never abort; calls = %q", calls)
	}
	if !strings.Contains(calls, "rtcWakeup") {
		t.Fatalf("expected RTC arm with keep-awake off; calls = %q", calls)
	}
	keepAwakeThreshold = 3 * time.Minute
}

// TestWakeDoesNotPullForward: a job whose time has not arrived must NOT be run on
// an early wake, and onWake must NOT arm the RTC (powerd is in Active right after a
// wake, where rtcWakeup is rejected). It waits for the natural readyToSuspend.
func TestWakeDoesNotPullForward(t *testing.T) {
	defer withTempState(t)()
	logp, restore := stubLipcCapture(t, true)
	defer restore()
	keepAwakeThreshold = 3 * time.Minute

	td := t.TempDir()
	jobsDir = td
	writeJob(t, td, "slow", "every 1h")
	setLastRun("slow", time.Now()) // next due ~1h out

	runsBefore := jobRunMarker(t, "slow")
	d := newDaemon()
	d.onWake()

	if jobRunMarker(t, "slow") != runsBefore {
		t.Fatal("job was pulled forward on wake despite not being due")
	}
	calls := readCalls(t, logp)
	if strings.Contains(calls, "rtcWakeup") {
		t.Fatalf("onWake must not arm RTC in Active state; calls = %q", calls)
	}
}

// TestWakeNeverArmsRTC: even for an imminent job, onWake does not arm or abort -
// it only runs due jobs and waits. (Arming/aborting happen in onReadyToSuspend.)
func TestWakeNeverArmsRTC(t *testing.T) {
	defer withTempState(t)()
	logp, restore := stubLipcCapture(t, true)
	defer restore()
	keepAwakeThreshold = 3 * time.Minute

	td := t.TempDir()
	jobsDir = td
	writeJob(t, td, "soon", "every 1m")
	setLastRun("soon", time.Now().Add(-30*time.Second)) // next due ~30s

	d := newDaemon()
	d.onWake()

	calls := readCalls(t, logp)
	if strings.Contains(calls, "rtcWakeup") || strings.Contains(calls, "abortSuspend") {
		t.Fatalf("onWake must not touch power props; calls = %q", calls)
	}
}

// jobRunMarker returns the job's last-run epoch (0 if never), used to detect runs.
func jobRunMarker(t *testing.T, name string) int64 {
	t.Helper()
	return getLastRun(name).Unix()
}

// TestSeedEveryJobs verifies seeding writes a start last-run for never-run "every"
// jobs, skips jobs that already ran, and never seeds "at"/"once".
func TestSeedEveryJobs(t *testing.T) {
	defer withTempState(t)()
	td := t.TempDir()
	jobsDir = td

	writeJob(t, td, "fresh", "every 10m") // never run -> should be seeded
	writeJob(t, td, "ran", "every 10m")   // already ran -> keep
	writeJob(t, td, "daily", "at 03:00")  // at -> never seed
	writeJob(t, td, "shot", "once 2030-01-01 00:00")

	ranStamp := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	setLastRun("ran", ranStamp)

	start := time.Now().Truncate(time.Second)
	seedEveryJobs(start)

	if got := getLastRun("fresh"); !got.Equal(start) {
		t.Fatalf("fresh seeded to %v, want %v", got, start)
	}
	if got := getLastRun("ran"); !got.Equal(ranStamp) {
		t.Fatalf("ran was reseeded to %v, want kept %v", got, ranStamp)
	}
	if getLastRun("daily").Unix() > 0 {
		t.Fatal("'at' job must not be seeded")
	}
	if getLastRun("shot").Unix() > 0 {
		t.Fatal("'once' job must not be seeded")
	}
}

// writeJob creates a minimal .job file due immediately (mtime in the past).
func writeJob(t *testing.T, dir, name, sched string) {
	t.Helper()
	p := filepath.Join(dir, name+".job")
	content := "schedule=" + sched + "\ncommand=/bin/true\nenabled=1\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(p, old, old)
}
