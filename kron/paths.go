package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

// The data directory layout, resolved once by initPaths:
//
//	<baseDir>/jobs.d/<name>.job    one file per job (schedule, command, enabled)
//	<baseDir>/state/<name>.last    last-run stamp; .log job output; .pid live job
//	<baseDir>/state/kron.lock      the daemon's singleton lock
//	<baseDir>/kron.log             the daemon's own log
var (
	baseDir  string
	jobsDir  string
	stateDir string
	logger   *log.Logger
	logFile  *cappedFile // the active kron.log writer (nil if it could not be opened)

	// logMaxBytes caps kron.log (oldest lines dropped). Set from the -logmax flag
	// in main before initPaths is called. 0 means unlimited.
	logMaxBytes int64 = 256 * 1024
)

func jobPath(name string) string    { return filepath.Join(jobsDir, name+".job") }
func lastPath(name string) string   { return filepath.Join(stateDir, name+".last") }
func logPath(name string) string    { return filepath.Join(stateDir, name+".log") }
func jobPidPath(name string) string { return filepath.Join(stateDir, name+".pid") }
func lockPath() string              { return filepath.Join(stateDir, "kron.lock") }

// initPaths resolves the data directory with precedence:
//
//	-dir flag  ->  $KRON_DIR  ->  the directory of the running binary
//
// The executable-relative default keeps the tool portable: no hardcoded paths,
// data lives beside the binary wherever it is installed. Override for FHS-style
// layouts (e.g. -dir /var/lib/kron).
func initPaths(flagDir string) error {
	dir := flagDir
	if dir == "" {
		dir = os.Getenv("KRON_DIR")
	}
	if dir == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		// Resolve the symlink that `kron setup` installs in /usr/bin, so the data
		// directory is the binary's real location, not /usr/bin.
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		dir = filepath.Dir(executable)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	baseDir = abs
	jobsDir = filepath.Join(baseDir, "jobs.d")
	stateDir = filepath.Join(baseDir, "state")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	initLog()
	return nil
}

// bestEffort wraps a writer so a failing Write (e.g. EPIPE on a stdout/stderr that
// KUAL has closed) is swallowed and reported as success. io.MultiWriter stops at
// the first writer error, so without this a broken stderr would keep every log
// line from reaching the file - exactly the log needed to diagnose the device.
type bestEffort struct{ target io.Writer }

func (writer bestEffort) Write(p []byte) (int, error) {
	writer.target.Write(p)
	return len(p), nil
}

// initLog points the package logger at kron.log and stderr. If the log file
// cannot be opened, logging continues to stderr alone.
func initLog() {
	var writer io.Writer = bestEffort{os.Stderr}
	path := filepath.Join(baseDir, "kron.log")
	if capped, err := newCappedFile(path, logMaxBytes); err == nil {
		logFile = capped
		writer = io.MultiWriter(bestEffort{capped}, bestEffort{os.Stderr})
	}
	logger = log.New(writer, "kron ", log.LstdFlags)
}

// logf writes one timestamped line to the log. It is a no-op before initPaths
// has run, so the CLI paths that never touch the data directory stay silent.
func logf(format string, a ...any) {
	if logger != nil {
		logger.Printf(format, a...)
	}
}
