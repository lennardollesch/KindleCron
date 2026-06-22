// KindleCron (kcron) is a standalone, cron-like task scheduler for
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

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

const usageText = `KindleCron (kcron) - cron-like scheduling that survives deep sleep on Kindles.

Usage:
  kcron [global flags] <command> [args]

Commands:
  daemon                         run the scheduler (default; start this at boot)
  stop                           tell a running daemon to shut down (cleans up its jobs)
  kill-jobs [NAME]               kill running job process group(s); all jobs, or just NAME.
                                 recovery for jobs orphaned by an unclean daemon death
  add [-timeout DUR] NAME SCHEDULE COMMAND...
                                 register or replace a job (-timeout kills it if it runs too long)
  remove NAME                    delete a job and its state
  enable NAME                    re-enable a disabled job (resumes scheduling)
  disable NAME                   stop scheduling a job without deleting it
  purge                          delete ALL jobs and their state (-y to skip prompt)
  clean-logs                     empty kcron.log and all per-job logs (-y to skip prompt)
  list                           show all jobs and when each next runs
  version                        print version
  help                           show this help

Global flags:
  -dir PATH    data directory for jobs/state/log
               (default: the binary's own directory; overrides $KCRON_DIR)
  -logmax KB   cap kcron.log at KB kilobytes, dropping oldest lines (0 = unlimited; default 256)
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
  -help        show this help and exit

Schedule formats (cron is the primary form; the "cron" keyword is optional):
  <cron expr>                  5- or 6-field cron, e.g. 0 9 * * 1 (Mondays 09:00).
                               6 fields add a leading seconds field: */30 * * * * *
                               Spell it out as "cron 0 9 * * 1" if you prefer.
  once YYYY-MM-DD HH:MM[:SS]   one-shot; runs once (catches up if missed) then self-removes
  every <N>[s|m|h|d]           convenience: fixed interval since last run, e.g. every 30m
  at HH:MM[,HH:MM,...]         convenience: daily at local times (same as cron M H * * *)

Examples:
  kcron add newYear  "0 0 0 1 1 *"           /mnt/us/app/newYear.sh
  kcron add weekly   "0 9 * * 1"             /mnt/us/app/weekly.sh
  kcron add poll     "*/30 * * * * *"        /mnt/us/app/poll.sh
  kcron add reminder "once 2026-06-15 14:30" /mnt/us/app/notify.sh

Autostart: start "kcron daemon" from your jailbreak's persistent startup hook.
`

func printUsage(w io.Writer) { fmt.Fprint(w, usageText) }

func main() {
	runtime.GOMAXPROCS(1)

	flagSet := flag.NewFlagSet("kcron", flag.ContinueOnError)
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
	)
	flagSet.StringVar(&dir, "dir", "", "data directory (default: binary's directory; overrides $KCRON_DIR)")
	flagSet.IntVar(&logMaxKB, "logmax", 256, "cap kcron.log at KB kilobytes (0 = unlimited)")
	flagSet.StringVar(&keepAwakeArg, "keepawake", "default", "keep device awake (no suspend) when the next job is due within this duration; a duration like 3m, 'default', or 'off' to disable")
	flagSet.StringVar(&wakeLeadArg, "wakelead", "15s", "wake this long BEFORE a job is due (safety margin so jobs are never missed; 0 to disable)")
	flagSet.StringVar(&jobTimeoutArg, "jobtimeout", "10m", "global max run time per job; a job exceeding it is killed (prevents a hung job pinning the device awake)")
	flagSet.BoolVar(&showVersion, "version", false, "print version and exit")
	flagSet.BoolVar(&showHelp, "help", false, "show help and exit")

	if err := flagSet.Parse(os.Args[1:]); err != nil {
		os.Exit(2) // flag has already reported the error / printed usage
	}
	if showVersion {
		fmt.Println(versionString())
		return
	}
	if showHelp {
		printUsage(os.Stdout)
		return
	}

	cmd := "daemon"
	args := flagSet.Args()
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

	// Go's flag package stops at the first non-flag argument, so global flags only
	// take effect when written BEFORE the subcommand (kcron -wakelead 30s daemon).
	// The natural form (kcron daemon -wakelead 30s) would otherwise silently ignore
	// them. For commands that take no positional arguments of their own, re-parse
	// the remaining args through the same flag set so global flags work in either
	// position. Commands with their own arguments/flags (add, remove, …) keep their
	// args untouched and are not re-parsed.
	switch cmd {
	case "daemon", "run", "list", "ls", "stop":
		if err := flagSet.Parse(args); err != nil {
			os.Exit(2)
		}
		args = flagSet.Args()
	}

	// info-only commands must not create the data directory
	switch cmd {
	case "version":
		fmt.Println(versionString())
		return
	case "help":
		printUsage(os.Stdout)
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
		runDaemon()
	case "add":
		cmdAdd(args)
	case "remove", "rm":
		cmdRemove(args)
	case "enable":
		cmdSetEnabled(args, true)
	case "disable":
		cmdSetEnabled(args, false)
	case "purge":
		cmdPurge(args)
	case "clean-logs":
		cmdCleanLogs(args)
	case "list", "ls":
		cmdList()
	case "stop":
		cmdStop()
	case "kill-jobs":
		cmdKillJobs(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		os.Exit(1)
	}
}
