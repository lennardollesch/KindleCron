// KindleCron (kron) is a standalone, cron-like task scheduler for
// jailbroken Kindles that survives deep sleep.

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

// fail reports a runtime failure (I/O, permissions, an unreachable device) on
// stderr and exits 1. See failUsage for the invalid-request case.
func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

// failUsage reports an invalid request (bad arguments or schedule) and exits 2,
// matching flag's own parse-error code. Distinct from fail (exit 1), which is for
// genuine runtime failures like I/O errors. This lets a program driving kron tell
// "you sent a bad request" apart from "the device failed".
func failUsage(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(2)
}

const usageText = `KindleCron (kron) - cron-like scheduling that survives deep sleep on Kindles.

Usage:
  kron [global flags] <command> [args]

Commands:
  daemon                         run the scheduler (default; start this at boot)
  stop                           tell a running daemon to shut down (cleans up its jobs)
  kill-jobs [NAME]               kill running job process group(s); all jobs, or just NAME.
                                 recovery for jobs orphaned by an unclean daemon death
  add [-timeout DUR] NAME SCHEDULE COMMAND...
                                 register or replace a job; NAME is a plain name (no '/' or
                                 leading '.'). -timeout kills the job if it runs too long
  remove NAME                    delete a job and its state
  enable NAME                    re-enable a disabled job (resumes scheduling)
  disable NAME                   stop scheduling a job without deleting it
  purge                          delete ALL jobs and their state (-y to skip prompt)
  clear-logs                     empty kron.log and all per-job logs (-y to skip prompt)
  list                           show all jobs and when each next runs
  setup                          symlink this binary into /usr/bin so a bare 'kron'
                                 works from anywhere and across reboots (run once)
  unlink                         remove the /usr/bin/kron symlink that setup created
  version                        print version
  help                           show this help

Global flags:
  -dir PATH    data directory for jobs/state/log
               (default: the binary's own directory; overrides $KRON_DIR)
  -logmax KB   cap kron.log at KB kilobytes, dropping oldest lines (0 = unlimited; default 256)
  -keepawake DUR
               keep the device awake (no suspend) when the next job is due within DUR
               (default 3m). A short interval like 'every 1m' cannot be reliably held
               across a suspend/wake cycle, so this trades battery for dependable
               short-interval runs. Use 'off' to always suspend regardless of interval.
  -wakelead DUR
               wake this long BEFORE a job is due (default 15s) as a safety margin, so a
               job is never missed by waking late. The wake timer runs the job on time.
  -jobtimeout DUR
               global max run time per job (default 10m). A job exceeding it is killed,
               so a hung job cannot pin the device awake. Override per job with -timeout.
  -version     print version and exit
  -eips        also show the result on the Kindle screen (centred, lower area) via
               eips. Meant for KUAL, where there is no terminal. Supported by
               daemon, stop, version, purge, kill-jobs, clear-logs, setup and unlink.
  -help        show this help and exit

Schedule formats (cron is the primary form; the "cron" keyword is optional):
  <cron expr>                  5- or 6-field cron, e.g. 0 9 * * 1 (Mondays 09:00).
                               6 fields add a leading seconds field: */30 * * * * *
                               Spell it out as "cron 0 9 * * 1" if you prefer.
  once YYYY-MM-DD HH:MM[:SS]   one-shot; runs once (catches up if missed) then self-removes
  every <N>[s|m|h|d]           convenience: fixed interval since last run, e.g. every 30m
  at HH:MM[,HH:MM,...]         convenience: daily at local times (same as cron M H * * *)

Examples:
  kron add newYear  "0 0 0 1 1 *"           /mnt/us/app/newYear.sh
  kron add weekly   "0 9 * * 1"             /mnt/us/app/weekly.sh
  kron add poll     "*/30 * * * * *"        /mnt/us/app/poll.sh
  kron add reminder "once 2026-06-15 14:30" /mnt/us/app/notify.sh

Autostart: start "kron daemon" from your jailbreak's persistent startup hook.

Exit codes: 0 success, 2 invalid arguments or schedule, 1 other failure.
`

func printUsage(out io.Writer) { fmt.Fprint(out, usageText) }

// emitVersion prints the version to stdout, and when eipsOut is set also shows it on
// the Kindle screen (see showResult). Off-device the on-screen part is a best-effort
// no-op and stdout still carries the version.
func emitVersion(eipsOut bool) {
	showText(eipsOut, "%s", versionString())
}

// main parses the global flags, applies them to the package-level settings, and
// dispatches the subcommand. Commands that need neither the data directory nor a
// log file run before initPaths, so `kron setup` works on a device where the data
// directory is not writable yet.
func main() {
	// The device is single-core and the daemon is I/O-bound, so extra Ps only add
	// scheduler overhead.
	runtime.GOMAXPROCS(1)
	ignoreSIGPIPE() // KUAL closes stdout; a write should not kill

	flagSet := flag.NewFlagSet("kron", flag.ContinueOnError)
	flagSet.SetOutput(os.Stderr)
	flagSet.Usage = func() { printUsage(os.Stderr) }
	var (
		dir           string
		logMaxKB      int
		keepAwakeArg  string
		wakeLeadArg   string
		jobTimeoutArg string
		showVersion   bool
		showHelp      bool
		eipsOut       bool
	)
	flagSet.StringVar(&dir, "dir", "", "data directory (default: binary's directory; overrides $KRON_DIR)")
	flagSet.IntVar(&logMaxKB, "logmax", 256, "cap kron.log at KB kilobytes (0 = unlimited)")
	flagSet.StringVar(&keepAwakeArg, "keepawake", "default", "keep device awake (no suspend) when the next job is due within this duration; a duration like 3m, 'default', or 'off' to disable")
	flagSet.StringVar(&wakeLeadArg, "wakelead", "15s", "wake this long BEFORE a job is due (safety margin so jobs are never missed; 0 to disable)")
	flagSet.StringVar(&jobTimeoutArg, "jobtimeout", "10m", "global max run time per job; a job exceeding it is killed (prevents a hung job pinning the device awake)")
	flagSet.BoolVar(&showVersion, "version", false, "print version and exit")
	flagSet.BoolVar(&showHelp, "help", false, "show help and exit")
	flagSet.BoolVar(&eipsOut, "eips", false, "also show the result on the Kindle screen via eips (daemon, stop, version, purge, kill-jobs, clear-logs, setup, unlink)")

	if err := flagSet.Parse(os.Args[1:]); err != nil {
		os.Exit(2) // flag has already reported the error / printed usage
	}

	cmd := "daemon"
	args := flagSet.Args()
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

	// Go's flag package stops at the first non-flag argument, so global flags only
	// take effect when written BEFORE the subcommand (kron -wakelead 30s daemon).
	// The natural form (kron daemon -wakelead 30s) would otherwise silently ignore
	// them. For commands that take no positional arguments of their own, re-parse
	// the remaining args through the same flag set so global flags work in either
	// position. Commands with their own arguments/flags (add, remove, …) keep their
	// args untouched and are not re-parsed.
	switch cmd {
	case "daemon", "run", "list", "ls", "stop", "version", "setup", "link", "unlink":
		if err := flagSet.Parse(args); err != nil {
			os.Exit(2)
		}
		args = flagSet.Args()
	}

	// -version / -help take precedence over the command and must not run it (e.g.
	// `kron stop -help` shows help, it does not stop the daemon). Checked here,
	// AFTER the re-parse, so they work in either position for no-arg commands.
	if showVersion {
		emitVersion(eipsOut)
		return
	}
	if showHelp {
		printUsage(os.Stdout)
		return
	}

	// commands that need neither the data directory nor its side effects
	switch cmd {
	case "version":
		emitVersion(eipsOut)
		return
	case "help":
		printUsage(os.Stdout)
		return
	case "setup", "link":
		cmdSetup(eipsOut)
		return
	case "unlink":
		cmdUnlink(eipsOut)
		return
	}

	if logMaxKB <= 0 {
		logMaxBytes = 0
	} else {
		logMaxBytes = int64(logMaxKB) * 1024
	}
	switch strings.ToLower(strings.TrimSpace(keepAwakeArg)) {
	case "", "default":
		// keep the built-in default
	case "off", "0":
		keepAwakeThreshold = keepAwakeDisabled
	default:
		duration, err := time.ParseDuration(keepAwakeArg)
		if err != nil || duration < 0 {
			fail("invalid -keepawake %q (want a duration like 3m, or 'off')", keepAwakeArg)
		}
		keepAwakeThreshold = duration
	}
	if wakeLeadParse, err := time.ParseDuration(wakeLeadArg); err != nil || wakeLeadParse < 0 {
		fail("invalid -wakelead %q (want a duration like 15s, or 0)", wakeLeadArg)
	} else {
		wakeLead = wakeLeadParse
	}
	if jobTimeoutParse, err := time.ParseDuration(jobTimeoutArg); err != nil || jobTimeoutParse <= 0 {
		fail("invalid -jobtimeout %q (want a positive duration like 10m)", jobTimeoutArg)
	} else {
		jobTimeoutMax = jobTimeoutParse
	}
	if err := initPaths(dir); err != nil {
		fail("init: %v", err)
	}

	switch cmd {
	case "daemon", "run":
		runDaemon(eipsOut)
	case "add":
		cmdAdd(args)
	case "remove", "rm":
		cmdRemove(args)
	case "enable":
		cmdSetEnabled(args, true)
	case "disable":
		cmdSetEnabled(args, false)
	case "purge":
		cmdPurge(args, eipsOut)
	case "clear-logs":
		cmdClearLogs(args, eipsOut)
	case "list", "ls":
		cmdList()
	case "stop":
		cmdStop(eipsOut)
	case "kill-jobs":
		cmdKillJobs(args, eipsOut)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(1)
	}
}
