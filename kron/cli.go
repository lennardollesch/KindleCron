package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lennardollesch/kindle-utils/eips"
)

const (
	// eipsVerticalFraction places the box in the lower area of the screen;
	// eipsPadding is the box's inner padding in grid cells.
	eipsVerticalFraction = 0.8
	eipsPadding          = 3
)

// drawOnScreen renders one line as a centred box in the lower area of the Kindle
// display via the shared kindle-utils eips package. Best-effort: it returns the
// error (e.g. off-device) for the caller to handle.
func drawOnScreen(text string) error {
	return eips.DrawBoxedText(text, eips.AlignCenter, eipsVerticalFraction, eipsPadding)
}

// drawIf shows text on the Kindle screen when eipsOut is set, reporting a draw
// failure to stderr. It lets the KUAL-facing commands confirm an outcome (success or
// failure) where there is no terminal to read stdout from.
func drawIf(eipsOut bool, text string) {
	if !eipsOut {
		return
	}
	if err := drawOnScreen(text); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

// showText prints a one-shot command's outcome to stdout and mirrors it to the
// screen via drawIf.
func showText(eipsOut bool, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	fmt.Println(line)
	drawIf(eipsOut, line)
}

// drawOrLog draws text on the Kindle screen (when eipsOut) and logs any draw
// failure. For the daemon, whose diagnostics go to the log rather than a terminal.
func drawOrLog(eipsOut bool, text string) {
	if !eipsOut {
		return
	}
	if err := drawOnScreen(text); err != nil {
		logf("eips: %v", err)
	}
}

// failShown reports a failure on the Kindle screen (when eipsOut) and on stderr,
// then exits 1. It pairs the screen and terminal messages so a KUAL-launched
// command always leaves a visible, consistent result.
func failShown(eipsOut bool, screenMsg, format string, a ...any) {
	drawIf(eipsOut, screenMsg)
	fail(format, a...)
}

// parseEipsFlag pulls a -eips flag out of args, returning the remaining args and
// whether it was present. Commands that take their own positional arguments parse
// it here (rather than through the global flag set) so it can follow the
// subcommand, e.g. `kron purge -y -eips`.
func parseEipsFlag(args []string) (rest []string, eipsOut bool) {
	for _, argument := range args {
		if argument == "-eips" || argument == "--eips" {
			eipsOut = true
			continue
		}
		rest = append(rest, argument)
	}
	return rest, eipsOut
}

// kronLinkPath is where setup installs the on-PATH symlink. /usr/bin is on PATH
// in every Kindle shell, so `kron` resolves from anywhere once linked.
const kronLinkPath = "/usr/bin/kron"

// cmdSetup makes `kron` callable from anywhere by symlinking the running binary
// into /usr/bin, so the KUAL menu and any other program can invoke a bare `kron`
// instead of a full path. It resolves the real binary itself (os.Executable), so
// it works wherever the user placed kron; the KUAL "Setup" action locates the
// binary and runs this, while a manual run is simply `<path>/kron setup`. The link
// survives reboots; a firmware update resets the root filesystem, so setup must be
// run again afterwards. Idempotent: an existing correct link is left as is.
func cmdSetup(eipsOut bool) {
	realPath, err := os.Executable()
	if err != nil {
		failShown(eipsOut, "kron: setup failed", "could not locate the kron binary: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(realPath); err == nil {
		realPath = resolved
	}
	switch info, statErr := os.Lstat(kronLinkPath); {
	case statErr == nil && info.Mode()&os.ModeSymlink != 0:
		if dest, _ := os.Readlink(kronLinkPath); dest == realPath {
			showText(eipsOut, "already set up: %s -> %s", kronLinkPath, realPath)
			return
		}
		// stale link from an earlier location; replaced below
	case statErr == nil:
		failShown(eipsOut, "kron: /usr/bin/kron exists", "%s already exists and is not a kron symlink; refusing to overwrite", kronLinkPath)
	}
	err = withWritableRoot(func() error {
		os.Remove(kronLinkPath) // clear a stale link; ignored if absent
		return os.Symlink(realPath, kronLinkPath)
	})
	if err != nil {
		failShown(eipsOut, "kron: setup failed", "could not create %s: %v", kronLinkPath, err)
	}
	showText(eipsOut, "linked %s -> %s", kronLinkPath, realPath)
	if !dirOnPath(filepath.Dir(kronLinkPath)) {
		fmt.Fprintf(os.Stderr, "warning: %s is not in PATH; add it so `kron` resolves\n", filepath.Dir(kronLinkPath))
	}
}

// cmdUnlink removes the /usr/bin/kron symlink that setup created, so `kron` is no
// longer resolved from PATH. A real file at that path (not the symlink) is left
// untouched.
func cmdUnlink(eipsOut bool) {
	info, err := os.Lstat(kronLinkPath)
	if err != nil {
		if os.IsNotExist(err) {
			showText(eipsOut, "not linked: nothing to remove")
			return
		}
		failShown(eipsOut, "kron: unlink failed", "could not inspect %s: %v", kronLinkPath, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		failShown(eipsOut, "kron: /usr/bin/kron not ours", "%s is not a symlink; refusing to remove", kronLinkPath)
	}
	if err := withWritableRoot(func() error { return os.Remove(kronLinkPath) }); err != nil {
		failShown(eipsOut, "kron: unlink failed", "could not remove %s: %v", kronLinkPath, err)
	}
	showText(eipsOut, "unlinked %s", kronLinkPath)
}

// withWritableRoot runs write with the root filesystem writable, restoring its
// original read-only state afterwards. The Kindle mounts "/" read-only, so the
// symlink in /usr/bin can only be created inside a brief read-write window. If the
// root is already writable (or its state is unknown), the mount state is left
// untouched and write runs as is.
func withWritableRoot(write func() error) error {
	if readOnly, known := rootReadonly(); known && readOnly {
		if err := remountRoot(true); err != nil {
			return fmt.Errorf("root filesystem is read-only and could not be remounted read-write (run as root, or 'mntroot rw' first): %w", err)
		}
		defer remountRoot(false)
		defer syncDisk() // flush to eMMC before the deferred remount to read-only
	}
	return write()
}

// dirOnPath reports whether dir is one of the entries in $PATH.
func dirOnPath(dir string) bool {
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == dir {
			return true
		}
	}
	return false
}

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
func cmdPurge(args []string, eipsOut bool) {
	args, localEips := parseEipsFlag(args)
	eipsOut = eipsOut || localEips
	_, yes := parseYesFlag(args)
	files := jobFiles()
	if len(files) == 0 {
		showText(eipsOut, "no jobs to purge")
		return
	}
	if !confirm(yes, fmt.Sprintf("delete all %d job(s) and their state in %s?", len(files), jobsDir)) {
		fmt.Println("aborted")
		return
	}
	purged := 0
	for _, filePath := range files {
		name := strings.TrimSuffix(filepath.Base(filePath), ".job")
		removeJob(name)
		purged++
	}
	showText(eipsOut, "purged %d job(s)", purged)
}

// cmdClearLogs empties the central kron.log and deletes all per-job logs. Jobs and
// their schedules are left untouched.
func cmdClearLogs(args []string, eipsOut bool) {
	args, localEips := parseEipsFlag(args)
	eipsOut = eipsOut || localEips
	_, yes := parseYesFlag(args)
	if !confirm(yes, "clear kron.log and all per-job logs?") {
		fmt.Println("aborted")
		return
	}
	// Central log: truncate in place so the active writer stays valid.
	if logFile != nil {
		if err := logFile.Truncate(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not clear %s: %v\n", filepath.Join(baseDir, "kron.log"), err)
		}
	} else {
		os.Truncate(filepath.Join(baseDir, "kron.log"), 0)
	}
	// Per-job logs: state/<name>.log
	logs, _ := filepath.Glob(filepath.Join(stateDir, "*.log"))
	removed := 0
	for _, logPath := range logs {
		if err := os.Remove(logPath); err == nil {
			removed++
		}
	}
	showText(eipsOut, "cleared kron.log and removed %d per-job log(s)", removed)
}

// cmdStop reads the daemon's PID from the lock file and asks it to shut down.
// The running daemon handles the signal in runDaemon: it logs "stopping",
// releases the lock and exits cleanly.
func cmdStop(eipsOut bool) {
	// Probe the flock before trusting the lock file: the kernel drops the lock when
	// the daemon dies, but the lock FILE (with a now-stale pid) stays behind. If no
	// live process holds it, no daemon is running, and signalling the recorded pid
	// could hit an unrelated process that has since been given that number.
	if !lockHeld(lockPath()) {
		failShown(eipsOut, "kron: not running", "not running (no live daemon holds %s)", lockPath())
	}
	// Read the pid only AFTER confirming the lock is held, so it belongs to the
	// daemon that holds it now, not a stale value from a previous instance.
	lockBytes, err := os.ReadFile(lockPath())
	if err != nil {
		failShown(eipsOut, "kron: not running", "not running (no lock file at %s)", lockPath())
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(lockBytes)))
	if err != nil || pid <= 0 {
		failShown(eipsOut, "kron: invalid lock file", "lock file %s has no valid pid", lockPath())
	}
	if pid == os.Getpid() {
		fail("refusing to signal self (pid %d)", pid)
	}
	if err := signalStop(pid); err != nil {
		failShown(eipsOut, "kron: stop failed", "could not stop daemon (pid %d): %v", pid, err)
	}
	showText(eipsOut, "sent stop to kron daemon (pid %d)", pid)
}

// cmdKillJobs terminates running job process groups recorded in state/<name>.pid.
// With no argument it targets every job; with a name, only that job. This is the
// recovery path for jobs orphaned by an unclean daemon death (kill -9, power
// loss): the daemon's normal stop already cleans up its own jobs. Stale pid files
// (process already gone) are simply removed.
func cmdKillJobs(args []string, eipsOut bool) {
	args, localEips := parseEipsFlag(args)
	eipsOut = eipsOut || localEips
	var targetName string // empty: every recorded job
	if len(args) >= 1 {
		targetName = args[0]
	}
	pids := jobPidFiles()
	if len(pids) == 0 {
		showText(eipsOut, "no running jobs recorded")
		return
	}
	killed, cleared := 0, 0
	for name, pid := range pids {
		if targetName != "" && name != targetName {
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
	if targetName != "" && killed == 0 && cleared == 0 {
		showText(eipsOut, "no running job named '%s'", targetName)
		return
	}
	showText(eipsOut, "done: %d killed, %d stale entries cleared", killed, cleared)
}

// jobPidFiles returns a map of job name -> recorded process-group pid for every
// state/<name>.pid file. Unreadable or malformed files are skipped.
func jobPidFiles() map[string]int {
	pidByJob := map[string]int{}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return pidByJob
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".pid")
		if name == "kron" {
			continue // reserved (daemon's own pid/lock namespace), not a job
		}
		content, err := os.ReadFile(filepath.Join(stateDir, entry.Name()))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
		if err == nil && pid > 1 {
			pidByJob[name] = pid
		}
	}
	return pidByJob
}

// validateJobName rejects names that are empty, would escape jobs.d, or collide
// with the daemon's reserved state files. A name becomes a <name>.job filename,
// so it must be a single plain filename component.
func validateJobName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("empty name")
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("%q must not contain a path separator", name)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("%q must not start with a dot", name)
	case name == "kron":
		return fmt.Errorf("%q is reserved for the daemon", name)
	}
	return nil
}

// cmdAdd registers a job, replacing any existing one of the same name. The
// schedule is parsed before anything is written, so an invalid request (exit 2)
// never leaves a partial job behind.
func cmdAdd(args []string) {
	// Optional leading "-timeout DUR" before NAME.
	var timeout time.Duration
	if len(args) >= 2 && args[0] == "-timeout" {
		duration, err := time.ParseDuration(args[1])
		if err != nil || duration <= 0 {
			failUsage("invalid -timeout %q (want a positive duration like 5m)", args[1])
		}
		timeout = duration
		args = args[2:]
	}
	if len(args) < 3 {
		failUsage("usage: kron add [-timeout DUR] NAME 'SCHEDULE' COMMAND...")
	}
	name, scheduleSpec, command := args[0], args[1], strings.Join(args[2:], " ")
	if err := validateJobName(name); err != nil {
		failUsage("invalid job name: %v", err)
	}
	parsed, err := parseSchedule(scheduleSpec)
	if err != nil {
		failUsage("invalid schedule: %v", err)
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
	if gap, short := shortInterval(parsed); short {
		fmt.Printf("  note: interval %s is below the keep-awake threshold (%s);\nwhile this job is the soonest due, the device stays at ready-to-suspend instead of sleeping, which results in higher battery usage.\n",
			gap.Round(time.Second), activeThreshold().Round(time.Second))
	}
}

// shortInterval reports whether a schedule fires often enough to keep the device
// awake (a gap between consecutive runs below the keep-awake threshold), and
// returns a representative short gap for the warning. For "every" the gap is the
// fixed interval; for "cron" it samples upcoming fires (see shortCronGap). "at"
// and "once" never qualify (their gaps are at least a day).
func shortInterval(sched schedule) (time.Duration, bool) {
	threshold := activeThreshold()
	if threshold <= 0 {
		return 0, false // mode off
	}
	switch sched.kind {
	case kindEvery:
		// interval > 0 also rejects a zero-value schedule (kindEvery, interval 0) left
		// by a parse error, so an unparsable job is never flagged keep-awake.
		if sched.interval > 0 && sched.interval < threshold {
			return sched.interval, true
		}
	case kindCron:
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

// cmdRemove deletes a job and its state. It is idempotent: removing a job that
// does not exist still succeeds, so a caller can clean up without checking first.
func cmdRemove(args []string) {
	if len(args) < 1 {
		failUsage("usage: kron remove NAME")
	}
	name := args[0]
	if err := validateJobName(name); err != nil {
		failUsage("invalid job name: %v", err)
	}
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
		failUsage("usage: kron %s NAME", verb)
	}
	name := args[0]
	if err := validateJobName(name); err != nil {
		failUsage("invalid job name: %v", err)
	}
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

// cmdList prints one row per job with its schedule and next run, appending a
// footer whenever a job's cadence keeps the device awake.
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
			activeThreshold().Round(time.Second))
	}
}

// jobListLine formats one job's row for `kron list` and reports whether it is a
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

// removeJob deletes a job's definition and every state file belonging to it.
// Missing files are ignored, which makes removal idempotent.
func removeJob(name string) {
	os.Remove(jobPath(name))
	os.Remove(lastPath(name))
	os.Remove(logPath(name))
}
