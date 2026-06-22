package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// confirm prompts the user unless `assumeYes` is set. Returns true to proceed.
func confirm(assumeYes bool, prompt string) bool {
	if assumeYes {
		return true
	}
	fmt.Printf("%s [y/N] ", prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes"
}

// parseYesFlag pulls a -y / --yes / --force flag out of args, returning the
// remaining args and whether the flag was present.
func parseYesFlag(args []string) (rest []string, yes bool) {
	for _, argument := range args {
		switch argument {
		case "-y", "--yes", "-f", "--force":
			yes = true
		default:
			rest = append(rest, argument)
		}
	}
	return rest, yes
}

// cmdPurge removes every job and all of its state (.job, .last, .log).
func cmdPurge(args []string) {
	_, yes := parseYesFlag(args)
	files := jobFiles()
	if len(files) == 0 {
		fmt.Printf("(no jobs in %s)\n", jobsDir)
		return
	}
	if !confirm(yes, fmt.Sprintf("delete all %d job(s) and their state in %s?", len(files), jobsDir)) {
		fmt.Println("aborted")
		return
	}
	n := 0
	for _, filePath := range files {
		name := strings.TrimSuffix(filepath.Base(filePath), ".job")
		removeJob(name)
		n++
	}
	fmt.Printf("purged %d job(s)\n", n)
}

// cmdCleanLogs empties the central kcron.log and deletes all per-job logs. Jobs and
// their schedules are left untouched.
func cmdCleanLogs(args []string) {
	_, yes := parseYesFlag(args)
	if !confirm(yes, "clear kcron.log and all per-job logs?") {
		fmt.Println("aborted")
		return
	}
	// Central log: truncate in place so the active writer stays valid.
	if logFile != nil {
		if err := logFile.Truncate(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not clear %s: %v\n", filepath.Join(baseDir, "kcron.log"), err)
		}
	} else {
		os.Truncate(filepath.Join(baseDir, "kcron.log"), 0)
	}
	// Per-job logs: state/<name>.log
	logs, _ := filepath.Glob(filepath.Join(stateDir, "*.log"))
	n := 0
	for _, logPath := range logs {
		if err := os.Remove(logPath); err == nil {
			n++
		}
	}
	fmt.Printf("cleared kcron.log and removed %d per-job log(s)\n", n)
}

// cmdStop reads the daemon's PID from the lock file and asks it to shut down.
// The running daemon handles the signal in runDaemon: it logs "stopping",
// releases the lock and exits cleanly.
func cmdStop() {
	lockBytes, err := os.ReadFile(lockPath())
	if err != nil {
		fail("not running (no lock file at %s)", lockPath())
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(lockBytes)))
	if err != nil || pid <= 0 {
		fail("lock file %s has no valid pid", lockPath())
	}
	if pid == os.Getpid() {
		fail("refusing to signal self (pid %d)", pid)
	}
	if err := signalStop(pid); err != nil {
		fail("could not stop daemon (pid %d): %v", pid, err)
	}
	fmt.Printf("sent stop to kcron daemon (pid %d)\n", pid)
}

// cmdKillJobs terminates running job process groups recorded in state/<name>.pid.
// With no argument it targets every job; with a name, only that job. This is the
// recovery path for jobs orphaned by an unclean daemon death (kill -9, power
// loss): the daemon's normal stop already cleans up its own jobs. Stale pid files
// (process already gone) are simply removed.
func cmdKillJobs(args []string) {
	var only string
	if len(args) >= 1 {
		only = args[0]
	}
	pids := jobPidFiles()
	if len(pids) == 0 {
		fmt.Println("no running jobs recorded")
		return
	}
	killed, cleared := 0, 0
	for name, pid := range pids {
		if only != "" && name != only {
			continue
		}
		if processGroupAlive(pid) {
			if killProcessGroupByPID(pid) {
				fmt.Printf("killed job '%s' (process group %d)\n", name, pid)
				killed++
			} else {
				fmt.Printf("could not kill job '%s' (process group %d)\n", name, pid)
			}
		} else {
			cleared++ // stale: process already gone
		}
		os.Remove(jobPidPath(name))
	}
	if only != "" && killed == 0 && cleared == 0 {
		fmt.Printf("no running job named '%s'\n", only)
		return
	}
	fmt.Printf("done: %d killed, %d stale entries cleared\n", killed, cleared)
}

// jobPidFiles returns a map of job name -> recorded process-group pid for every
// state/<name>.pid file. Unreadable or malformed files are skipped.
func jobPidFiles() map[string]int {
	out := map[string]int{}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".pid")
		if name == "kcron" {
			continue // reserved (daemon's own pid/lock namespace), not a job
		}
		entryBytes, err := os.ReadFile(filepath.Join(stateDir, entry.Name()))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(entryBytes)))
		if err == nil && pid > 1 {
			out[name] = pid
		}
	}
	return out
}

func cmdAdd(args []string) {
	// Optional leading "-timeout DUR" before NAME.
	var timeout time.Duration
	if len(args) >= 2 && args[0] == "-timeout" {
		duration, err := time.ParseDuration(args[1])
		if err != nil || duration <= 0 {
			fail("invalid -timeout %q (want a positive duration like 5m)", args[1])
		}
		timeout = duration
		args = args[2:]
	}
	if len(args) < 3 {
		fail("usage: kcron add [-timeout DUR] NAME 'SCHEDULE' COMMAND...")
	}
	name, scheduleSpec, command := args[0], args[1], strings.Join(args[2:], " ")
	parsed, err := parseSchedule(scheduleSpec)
	if err != nil {
		fail("invalid schedule: %v", err)
	}
	content := fmt.Sprintf("schedule=%s\ncommand=%s\nenabled=1\n", scheduleSpec, command)
	if timeout > 0 {
		content += fmt.Sprintf("timeout=%s\n", timeout)
	}
	if err := atomicWrite(jobPath(name), []byte(content)); err != nil {
		fail("write failed: %v", err)
	}
	fmt.Printf("added '%s'  (%s)  -> %s\n", name, scheduleSpec, command)
	if timeout > 0 {
		fmt.Printf("  timeout: %s (job killed if it runs longer)\n", timeout)
	}
	if d, short := shortInterval(parsed); short {
		fmt.Printf("  note: interval %s is below the keep-awake threshold (%s);\nwhile this job is the soonest due, the device stays at ready-to-suspend instead of sleeping, which results in higher battery usage.\n",
			d.Round(time.Second), keepAwakeThresholdForCLI().Round(time.Second))
	}
}

// shortInterval reports whether a schedule fires often enough to keep the device
// awake (a gap between consecutive runs below the keep-awake threshold), and
// returns a representative short gap for the warning. For "every" the gap is the
// fixed interval; for "cron" it samples upcoming fires (see shortCronGap). "at"
// and "once" never qualify (their gaps are at least a day).
func shortInterval(sched schedule) (time.Duration, bool) {
	threshold := keepAwakeThresholdForCLI()
	if threshold <= 0 {
		return 0, false // mode off
	}
	switch sched.kind {
	case kEvery:
		if sched.interval < threshold {
			return sched.interval, true
		}
	case kCron:
		return shortCronGap(sched.cron, threshold, time.Now())
	}
	return 0, false
}

// shortCronGap reports whether a cron expression would currently keep the device
// awake, returning the gap responsible. This mirrors the daemon's keep-awake
// condition (see onReadyToSuspend): the device is only held awake when the NEXT
// fire is within the threshold, AND the fire after that is too, so it is kept up
// back-to-back rather than waking once and sleeping again.
//
// Gating on the next fire matters because a cron can pack short gaps into a rare
// burst, e.g. "* * * 1 * *" (every second, but only on the 1st) or
// "*/10 0 14 22 6 *" (every 10s for a minute, once a year). Between bursts the
// next fire is days or months out, so the device sleeps normally and the job must
// not be flagged. Sampling consecutive gaps alone (ignoring how far off they are)
// would wrongly flag these.
func shortCronGap(expr cronExpr, threshold time.Duration, from time.Time) (time.Duration, bool) {
	next := expr.next(from)
	if next.IsZero() || next.Sub(from) >= threshold {
		return 0, false // next fire too far out: the device sleeps until then
	}
	following := expr.next(next)
	if following.IsZero() {
		return 0, false
	}
	if gap := following.Sub(next); gap < threshold {
		return gap, true
	}
	return 0, false // a single imminent fire, then a long gap: wake once, then sleep
}

// keepAwakeThresholdForCLI returns the same threshold the daemon would use, for
// warnings in add/list.
func keepAwakeThresholdForCLI() time.Duration {
	return activeThreshold()
}

func cmdRemove(args []string) {
	if len(args) < 1 {
		fail("usage: kcron remove NAME")
	}
	name := args[0]
	removeJob(name)
	fmt.Printf("removed '%s'\n", name)
}

// cmdSetEnabled flips a job's enabled flag in its .job file. The daemon re-reads
// job files on every evaluation, so the change takes effect without a restart: a
// disabled job stops being scheduled, an enabled one resumes.
func cmdSetEnabled(args []string, enabled bool) {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	if len(args) < 1 {
		fail("usage: kcron %s NAME", verb)
	}
	name := args[0]
	if err := setJobEnabled(name, enabled); err != nil {
		if os.IsNotExist(err) {
			fail("no job named '%s'", name)
		}
		fail("%s '%s' failed: %v", verb, name, err)
	}
	fmt.Printf("%sd '%s'\n", verb, name)
}

// setJobEnabled rewrites <name>.job with the enabled flag set to value, leaving
// every other line untouched. An existing enabled= line is replaced in place; if
// none exists, one is appended.
func setJobEnabled(name string, enabled bool) error {
	path := jobPath(name)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	value := "0"
	if enabled {
		value = "1"
	}
	var out []string
	found := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if equalIndex := strings.IndexByte(trimmed, '='); equalIndex >= 0 && strings.TrimSpace(trimmed[:equalIndex]) == "enabled" {
			out = append(out, "enabled="+value)
			found = true
			continue
		}
		out = append(out, line)
	}
	content := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if !found {
		content += "\nenabled=" + value
	}
	return atomicWrite(path, []byte(content+"\n"))
}

func cmdList() {
	now := time.Now()
	paths := jobFiles()
	if len(paths) == 0 {
		fmt.Printf("(no jobs in %s)\n", jobsDir)
		return
	}
	anyShort := false
	for _, path := range paths {
		line, short := jobListLine(readJob(path), now)
		anyShort = anyShort || short
		fmt.Println(line)
	}
	if anyShort {
		fmt.Printf("\n[keep awake] jobs run below the %s threshold:\nwhile one is the soonest due, the device stays at ready-to-suspend instead of sleeping (more battery use).\n",
			keepAwakeThresholdForCLI().Round(time.Second))
	}
}

// jobListLine formats one job's row for `kcron list` and reports whether it is a
// short-interval keep-awake job (so the caller can print the footer note). Pure
// formatting kept separate from printing so it can be unit-tested.
func jobListLine(entry job, now time.Time) (line string, short bool) {
	var status string
	switch {
	case entry.schedErr != nil:
		status = "INVALID: " + entry.schedErr.Error()
	case !entry.enabled:
		status = "disabled"
	default:
		if nextDue, ok := entry.sched.nextDue(getLastRun(entry.name), entry.mtime, now); ok {
			status = fmt.Sprintf("next: %ds", int(nextDue.Sub(now)/time.Second))
		} else {
			status = "done"
		}
	}
	mark := ""
	if _, isShort := shortInterval(entry.sched); isShort && entry.schedErr == nil && entry.enabled {
		mark = "  [keep awake]"
		short = true
	}
	timeoutInfo := ""
	if entry.timeout > 0 {
		timeoutInfo = fmt.Sprintf("  timeout %s", entry.timeout)
	}
	line = fmt.Sprintf("%-20s %-26s %s%s%s", entry.name, entry.schedRaw, status, timeoutInfo, mark)
	return line, short
}

func removeJob(name string) {
	os.Remove(jobPath(name))
	os.Remove(lastPath(name))
	os.Remove(logPath(name))
}
