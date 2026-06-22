package main

import (
	"bufio"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	powerd = "com.lab126.powerd"
	events = "goingToScreenSaver,wakeupFromSuspend,readyToSuspend"
)

var (
	minDelta    = 60              // never arm the RTC closer than this (s)
	maxDelta    = 86400           // longest single sleep; heartbeat + fallback (s)
	retryDelay  = 5 * time.Second // wait before re-subscribing if the stream ends
	armAttempts = 4               // retries for setting rtcWakeup near suspend

	// wakeLead is a deliberate safety margin: the RTC is armed to fire this much
	// BEFORE a job is due, so the device is reliably awake (in Active, then drifting
	// toward readyToSuspend) by the time the job's moment arrives, and the wake
	// timer fires it on time. Waking a little early and waiting is always safe;
	// waking late risks suspending through the job's slot and missing it entirely.
	// Settable via -wakelead; set to 0 to disable.
	wakeLead = 15 * time.Second
)

// daemon holds the scheduler's runtime state.
//
// The keep-awake mode uses powerd's abortSuspend, a write-only trigger that is
// only settable during readyToSuspend. There is no "currently holding the device
// awake" state to track and no cleanup to guarantee on exit: if kcron dies, no
// further aborts are sent and powerd completes the next suspend on its own.
type daemon struct {
	// deferring is true while suspend is being aborted for a running or imminent
	// job. It only de-duplicates the running-job log line and marks the transition
	// back to allowing suspend; it is not a resource that needs release.
	deferring bool

	// Job tracking. `pending` counts jobs that have been dispatched but not yet
	// finished; it is incremented SYNCHRONOUSLY in dispatchDue before the goroutine
	// starts, so anyJobRunning() is correct the instant a job is dispatched (no
	// race window where onReadyToSuspend could suspend on a just-started job).
	// `processes` holds the live process for each running job (registered once it
	// has actually started) so shutdown can terminate them. Guarded by mu.
	mu        sync.Mutex
	pending   int
	processes map[int]*exec.Cmd
	nextJobID int
}

// jobDispatched marks a job as dispatched (about to run) and returns its id. Call
// synchronously before starting the goroutine.
func (dm *daemon) jobDispatched() int {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.pending++
	id := dm.nextJobID
	dm.nextJobID++
	return id
}

// jobProcessStarted records the live process for a dispatched job.
func (dm *daemon) jobProcessStarted(id int, cmd *exec.Cmd) {
	dm.mu.Lock()
	if dm.processes == nil {
		dm.processes = make(map[int]*exec.Cmd)
	}
	dm.processes[id] = cmd
	dm.mu.Unlock()
}

// jobFinished clears a job's tracking (both the pending count and any process).
func (dm *daemon) jobFinished(id int) {
	dm.mu.Lock()
	dm.pending--
	delete(dm.processes, id)
	dm.mu.Unlock()
}

func (dm *daemon) anyJobRunning() bool {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	return dm.pending > 0
}

// hasRunningProcess reports whether at least one dispatched job has a live process
// registered (started). Distinct from anyJobRunning, which also counts jobs that
// are dispatched but not yet started.
func (dm *daemon) hasRunningProcess() bool {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	return len(dm.processes) > 0
}

// killRunningJobs terminates every still-running job process. Called on shutdown
// so jobs do not survive the daemon as orphans. Returns the number killed.
func (dm *daemon) killRunningJobs() int {
	dm.mu.Lock()
	running := make([]*exec.Cmd, 0, len(dm.processes))
	for _, cmd := range dm.processes {
		running = append(running, cmd)
	}
	dm.mu.Unlock()
	killed := 0
	for _, cmd := range running {
		if killProcessGroup(cmd) {
			killed++
		}
	}
	return killed
}

func newDaemon() *daemon {
	return &daemon{}
}

func runDaemon() {
	// Single-instance via an exclusive file lock (flock on Unix). This is
	// stale-proof: the lock is released by the kernel when the process dies, even
	// on a crash, so there is no leftover pidfile to reason about. Covers both
	// `daemon` and `run`.
	lock, ok := acquireSingleton(lockPath())
	if !ok {
		logf("another instance is already running; exiting")
		return
	}
	defer lock.Close()

	logf("scheduler starting (data dir %s)", baseDir)

	// Anchor never-run 'every' jobs to the daemon start by seeding a real last-run
	// stamp now. This makes the first run land one interval after start (intuitive)
	// and turns the scheduling grid into a stable persisted timestamp rather than a
	// value derived from the .job mtime or an in-memory start time.
	seedEveryJobs(time.Now())

	dm := newDaemon()
	if threshold := activeThreshold(); threshold > 0 {
		logf("keep-awake threshold: %s (jobs due sooner stay awake instead of sleeping)", threshold)
	}

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, shutdownSignals()...)
	go func() {
		<-signalCh
		logf("stopping")
		// Terminate any still-running job processes so they do not survive the
		// daemon as orphans (a backgrounded job whose parent exits would otherwise
		// be reparented to init and keep running). abortSuspend needs no cleanup: it
		// auto-expires once we stop sending it.
		if n := dm.killRunningJobs(); n > 0 {
			logf("terminated %d running job(s) on shutdown", n)
		}
		lock.Close()
		os.Exit(0)
	}()

	// wakeTimer fires when the soonest job becomes due while the device is awake.
	// powerd events handle the suspend path; this handles everything in between,
	// so jobs no longer wait for the next wake to run. A resting timer costs
	// nothing (no polling) and does not keep the device from suspending: on
	// suspend the CPU stops and the timer with it, and the RTC takes over.
	wakeTimer := time.NewTimer(time.Hour)
	wakeTimer.Stop()
	rearm := func() {
		now := time.Now()
		if soonest, have := soonestDue(now); have {
			wait := soonest.Sub(now)
			if wait < 0 {
				wait = 0
			}
			wakeTimer.Reset(wait)
		} else {
			wakeTimer.Stop()
		}
	}
	// drainTimer empties a fired timer's channel so a later Reset is clean.
	drainTimer := func() {
		select {
		case <-wakeTimer.C:
		default:
		}
	}

	rearm() // catch anything already due at startup

	// Outer loop re-subscribes if powerd restarts (firmware update, crash, ...).
	for {
		cmd := exec.Command("lipc-wait-event", "-m", powerd, events)
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			logf("cannot start lipc-wait-event: %v - retrying in %s", err, retryDelay)
			time.Sleep(retryDelay)
			continue
		}

		// Read the event stream in a goroutine so the main loop can also wait on
		// the wake timer. A closed channel signals the stream ended.
		lines := make(chan string)
		go func() {
			scanner := bufio.NewScanner(out) // blocks while idle: zero CPU
			for scanner.Scan() {
				lines <- strings.TrimSpace(scanner.Text())
			}
			close(lines)
		}()

		streamUp := true
		for streamUp {
			select {
			case line, ok := <-lines:
				if !ok {
					streamUp = false // stream ended; fall through to re-subscribe
					break
				}
				dm.handleEvent(line)
				rearm() // events change due-times (jobs ran / RTC reprogrammed)
			case <-wakeTimer.C:
				drainTimer()
				dm.dispatchDue()
				rearm()
			}
		}

		cmd.Wait()
		logf("event stream ended - re-subscribing in %s", retryDelay)
		time.Sleep(retryDelay)
	}
}

func (dm *daemon) handleEvent(line string) {
	switch {
	case strings.HasPrefix(line, "goingToScreenSaver"):
		logf("event goingToScreenSaver")
	case strings.HasPrefix(line, "wakeupFromSuspend"):
		logf("event wakeupFromSuspend")
		dm.onWake()
	case strings.HasPrefix(line, "readyToSuspend"):
		logf("event readyToSuspend")
		dm.onReadyToSuspend()
	}
}

// onWake handles wakeupFromSuspend. It runs only jobs that are actually due now
// (no pull-forward). It does NOT arm the RTC here: right after a wake powerd is in
// the Active state, where rtcWakeup (like abortSuspend) is rejected, so an arm
// would fail. Instead it does nothing power-related and lets powerd reach
// readyToSuspend naturally; onReadyToSuspend then decides arm-vs-stay-awake from a
// state where the property is settable. The wake timer still runs any imminent job
// in the meantime.
func (dm *daemon) onWake() {
	dm.dispatchDue() // only genuinely-due jobs; no slack, no pull-forward

	now := time.Now()
	soonest, have := soonestDue(now)
	if !have {
		return
	}
	logf("wake: next job in %s - waiting for readyToSuspend to decide", soonest.Sub(now).Round(time.Second))
}

// onReadyToSuspend decides, on every readyToSuspend, between aborting the suspend
// (for a running or imminent job) and arming the RTC to sleep. Aborting uses
// powerd's abortSuspend, which restarts the short ReadyToSuspend countdown without
// kicking the device back to the Active state; powerd then re-emits readyToSuspend
// and kcron re-evaluates. No state is "held": if the job is still imminent next time,
// it aborts again; once it is far enough out, it stops aborting and the device
// suspends (it also arms the RTC so the device wakes for the job after sleeping).
func (dm *daemon) onReadyToSuspend() {
	now := time.Now()

	// Highest priority: never suspend while a job is still running, or it would be
	// frozen mid-execution. Abort the suspend and re-evaluate on the next
	// readyToSuspend. The per-job timeout (enforced in runJob) guarantees this
	// cannot hold the device awake forever: a hung job is killed, anyJobRunning
	// drops to false, and the next readyToSuspend proceeds normally.
	if dm.anyJobRunning() {
		if err := requestAbortSuspend(); err != nil {
			logf("keep-awake: abortSuspend rejected while job running (%v) - device may suspend", err)
		} else {
			if !dm.deferring {
				logf("keep-awake: a job is still running - aborting suspend until it finishes")
			}
			dm.deferring = true
			return
		}
	}

	soonest, have := soonestDue(now)
	threshold := activeThreshold()

	if have && threshold > 0 && soonest.Sub(now) < threshold {
		// Next job is too soon to survive a suspend/wake roundtrip reliably; tell
		// powerd to abort this suspend so the wake timer can fire the job while the
		// device is still up.
		if err := requestAbortSuspend(); err != nil {
			logf("keep-awake: abortSuspend rejected (%v) - falling back to suspend", err)
		} else {
			// Log on every abort, not just the first of a deferring run, so each
			// readyToSuspend is paired with the reason it did not lead to a suspend.
			logf("keep-awake: %s < threshold %s - abortSuspend",
				soonest.Sub(now).Round(time.Second), threshold)
			dm.deferring = true
			return
		}
	}

	// Normal path: allow suspend and arm the RTC.
	if dm.deferring {
		dm.deferring = false
		logf("keep-awake: next job far enough out; allowing suspend")
	}
	dm.programNextWakeup(now, soonest, have)
}

// dispatchDue runs due jobs in the BACKGROUND (one goroutine each) and tracks them
// via the pending count, so the event loop stays responsive while a job runs. This
// is what lets onReadyToSuspend keep the device awake (abortSuspend) for the whole
// duration of a long job instead of the daemon blocking in cmd.Run(), where it
// could not answer powerd and the device would suspend mid-job. last-run is
// committed BEFORE the goroutine starts so the same job is not re-dispatched on the
// next timer tick while it is still running.
func (dm *daemon) dispatchDue() {
	now := time.Now()
	for _, path := range jobFiles() {
		entry := readJob(path)
		if !entry.enabled || entry.command == "" || entry.schedErr != nil {
			continue
		}
		last := getLastRun(entry.name)
		if !entry.sched.isDue(last, entry.mtime, now) {
			continue
		}
		// Commit grid/state now, synchronously, so re-evaluation won't re-fire it.
		if entry.sched.kind == kOnce {
			os.Remove(path)
			os.Remove(lastPath(entry.name))
			logf("one-shot '%s' fired and removed", entry.name)
		} else {
			setLastRun(entry.name, entry.sched.fireStamp(last, entry.mtime, now))
		}
		id := dm.jobDispatched() // synchronous: anyJobRunning() true immediately
		go func(def job, id int) {
			defer dm.jobFinished(id)
			runJob(def, func(cmd *exec.Cmd) func() {
				dm.jobProcessStarted(id, cmd)
				// Record the process-group id (== leader pid) so an external
				// `kcron kill-jobs` can terminate this job even if the daemon later
				// dies uncleanly and its in-memory tracking is lost.
				writeJobPid(def.name, cmd.Process.Pid)
				return func() { os.Remove(jobPidPath(def.name)) }
			})
		}(entry, id)
	}
}

// seedEveryJobs writes last-run = start for every enabled, never-run "every" job,
// so its scheduling grid is anchored on the daemon start. Only "every" jobs are
// seeded: "at" and "once" have absolute time references where a synthetic last-run
// would be wrong. Jobs that already have a last-run (ran before, survived a
// restart) are left untouched, so restarting the daemon does not shift their grid.
func seedEveryJobs(start time.Time) {
	for _, path := range jobFiles() {
		entry := readJob(path)
		if entry.schedErr != nil || entry.sched.kind != kEvery {
			continue
		}
		if getLastRun(entry.name).Unix() > 0 {
			continue // already has a real last-run; keep its grid
		}
		setLastRun(entry.name, start)
		logf("seeded '%s' (every %s) anchored to start", entry.name, entry.sched.interval)
	}
}

// soonestDue returns the earliest next-due time across all enabled, valid jobs,
// evaluated at `now`. have=false means no job will ever run again.
func soonestDue(now time.Time) (soonest time.Time, have bool) {
	for _, path := range jobFiles() {
		entry := readJob(path)
		if !entry.enabled || entry.schedErr != nil {
			continue
		}
		nextDue, ok := entry.sched.nextDue(getLastRun(entry.name), entry.mtime, now)
		if !ok {
			continue
		}
		if !have || nextDue.Before(soonest) {
			soonest, have = nextDue, true
		}
	}
	return soonest, have
}

// programNextWakeup arms the RTC for the soonest upcoming job. Called repeatedly
// within a readyToSuspend cluster; arming again with a smaller delta each time is
// harmless (powerd uses the latest value).
//
// The armed delta is (time-until-due - wakeLead), so the device wakes a deliberate
// margin early. The seconds are ceil-rounded so the only early-wake margin is the
// explicit wakeLead, not sub-second truncation.
func (dm *daemon) programNextWakeup(now, soonest time.Time, have bool) int {
	delta := computeWakeDelta(now, soonest, have)
	armRTC(delta)
	return delta
}

// computeWakeDelta returns the RTC delta (seconds) for waking `wakeLead` before
// `soonest`, clamped to [minDelta, maxDelta], ceil-rounded to whole seconds. When
// !have, it returns maxDelta (a heartbeat/fallback sleep).
func computeWakeDelta(now, soonest time.Time, have bool) int {
	delta := maxDelta
	if have {
		untilDue := soonest.Sub(now) - wakeLead
		delta = int((untilDue + time.Second - 1) / time.Second) // ceil
	}
	if delta < minDelta {
		delta = minDelta
	}
	if delta > maxDelta {
		delta = maxDelta
	}
	return delta
}

// armRTC sets powerd's rtcWakeup, retrying transient failures. (rtcWakeup is
// effectively write-only on the device, so it is not read back.)
func armRTC(delta int) {
	var err error
	for attempt := 1; attempt <= armAttempts; attempt++ {
		err = exec.Command("lipc-set-prop", "-i", powerd, "rtcWakeup", strconv.Itoa(delta)).Run()
		if err == nil {
			logf("rtc armed for %ds", delta)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	logf("rtc arm FAILED after %d attempts: %v (delta=%ds)", armAttempts, err, delta)
}
