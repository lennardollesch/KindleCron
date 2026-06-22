package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

var (
	baseDir  string
	jobsDir  string
	stateDir string
	logger   *log.Logger
	logFile  *cappedFile // the active kcron.log writer (nil if it could not be opened)

	// logMaxBytes caps kcron.log (oldest lines dropped). Set from the -logmax flag
	// in main before initPaths is called. 0 means unlimited.
	logMaxBytes int64 = 256 * 1024
)

func jobPath(name string) string    { return filepath.Join(jobsDir, name+".job") }
func lastPath(name string) string   { return filepath.Join(stateDir, name+".last") }
func logPath(name string) string    { return filepath.Join(stateDir, name+".log") }
func jobPidPath(name string) string { return filepath.Join(stateDir, name+".pid") }
func lockPath() string              { return filepath.Join(stateDir, "kcron.lock") }

// initPaths resolves the data directory with precedence:
//
//	-dir flag  ->  $KCRON_DIR  ->  the directory of the running binary
//
// The executable-relative default keeps the tool portable: no hardcoded paths,
// data lives beside the binary wherever it is installed. Override for FHS-style
// layouts (e.g. -dir /var/lib/kcron).
func initPaths(flagDir string) error {
	dir := flagDir
	if dir == "" {
		dir = os.Getenv("KCRON_DIR")
	}
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if relativePath, e := filepath.EvalSymlinks(exe); e == nil {
			exe = relativePath
		}
		dir = filepath.Dir(exe)
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

func initLog() {
	var writer io.Writer = os.Stderr
	path := filepath.Join(baseDir, "kcron.log")
	if capped, err := newCappedFile(path, logMaxBytes); err == nil {
		logFile = capped
		writer = io.MultiWriter(os.Stderr, capped)
	}
	logger = log.New(writer, "kcron ", log.LstdFlags)
}

func logf(format string, a ...any) {
	if logger != nil {
		logger.Printf(format, a...)
	}
}
