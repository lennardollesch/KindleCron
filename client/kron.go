// Package client drives KindleCron (kron), the external scheduler that
// survives a Kindle's deep sleep, from another program.
//
// kron is a standalone binary: its daemon must run as its own
// long-lived process, and jobs are registered by invoking the binary. This
// package wraps those invocations so a program can locate kron, make sure the
// daemon is running, and add or remove jobs without shelling out by hand.
//
// # Success contract
//
//	0  the request was validated and applied (job written or removed)
//	2  the request was invalid - a malformed schedule or bad arguments;
//	   surfaced as ErrInvalidRequest
//	other non-zero  some other failure (I/O, etc.); surfaced as *CommandError
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// binaryName is the kron executable's name on PATH and on disk.
const binaryName = "kron"

// defaultSearchRoot is where a Kindle deployment keeps kron when it is not on
// PATH: the user partition. Overridable with WithSearchRoot.
const defaultSearchRoot = "/mnt/us"

// ExitInvalidRequest is the exit code kron returns for a malformed schedule or
// bad arguments. It is part of the contract this package relies on and is
// mapped to ErrInvalidRequest.
const ExitInvalidRequest = 2

var (
	// ErrBinaryNotFound reports that the kron executable could not be located,
	// neither on PATH nor under the search root. A program will usually turn
	// this into a "please install kron" hint for the user.
	ErrBinaryNotFound = errors.New("kron: binary not found on PATH or under the search root")

	// ErrInvalidRequest reports that kron rejected the request as malformed
	// (exit code ExitInvalidRequest), e.g. an unparsable schedule. It wraps
	// into the *CommandError returned by Add/Remove, so errors.Is detects it.
	ErrInvalidRequest = errors.New("kron: invalid schedule or arguments")
)

// CommandError is returned when a kron invocation ran but exited non-zero.
type CommandError struct {
	Op       string   // the operation, e.g. "add" or "remove"
	Args     []string // the argument vector passed to kron
	ExitCode int      // kron's exit code
	Stderr   string   // trimmed stderr, if any
	sentinel error    // ErrInvalidRequest for a known code, else nil
}

// Error formats the failed operation, its exit code, and any stderr output.
func (commandError *CommandError) Error() string {
	if commandError.Stderr != "" {
		return fmt.Sprintf("kron %s: exit %d: %s", commandError.Op, commandError.ExitCode, commandError.Stderr)
	}
	return fmt.Sprintf("kron %s: exit %d", commandError.Op, commandError.ExitCode)
}

// Unwrap exposes the sentinel (ErrInvalidRequest) so errors.Is can match it.
func (commandError *CommandError) Unwrap() error { return commandError.sentinel }

// Job is a scheduler entry to register with Add.
type Job struct {
	Name     string        // unique job name; re-adding the same name replaces it
	Schedule Schedule      // when the job fires (see Once, Every, At, Cron)
	Command  []string      // program and arguments kron runs when the job fires
	Timeout  time.Duration // optional per-run limit; 0 uses kron's own default
}

// addArgs builds the argument vector for `kron add`, in the order kron's own
// parser expects: flags, then name, schedule, and the command with its arguments.
func (job Job) addArgs() []string {
	args := []string{"add"}
	if job.Timeout > 0 {
		args = append(args, "-timeout", job.Timeout.String())
	}
	args = append(args, job.Name, job.Schedule.String())
	return append(args, job.Command...)
}

// runResult holds the outcome of one kron invocation.
type runResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// runner executes kron and reports its result. The error is non-nil only when
// kron could not be run at all (missing binary, cancelled context); a non-zero
// exit is reported in runResult.exitCode, not as an error.
type runner func(ctx context.Context, binary string, args []string) (runResult, error)

// daemonStarter launches "kron daemon" detached from the caller.
type daemonStarter func(binary string) error

// Client invokes a resolved kron binary. Create one with New. A Client is safe
// for concurrent use: it holds no mutable state after construction.
type Client struct {
	binary string
	run    runner
	start  daemonStarter
}

// Option configures New.
type Option func(*config)

// config holds the options New assembles before constructing a Client.
type config struct {
	binary     string
	searchRoot string
	run        runner
	start      daemonStarter
}

// WithBinary uses an explicit path to the kron executable and skips discovery.
func WithBinary(path string) Option {
	return func(cfg *config) { cfg.binary = path }
}

// WithSearchRoot overrides the directory searched for kron when it is not on
// PATH (default "/mnt/us").
func WithSearchRoot(dir string) Option {
	return func(cfg *config) { cfg.searchRoot = dir }
}

// New locates the kron binary and returns a Client for it. Discovery consults
// PATH first (where "kron setup" installs a symlink), then searches under the
// search root. It returns ErrBinaryNotFound if kron cannot be located; pass
// WithBinary to skip discovery entirely.
func New(opts ...Option) (*Client, error) {
	cfg := config{searchRoot: defaultSearchRoot, run: execRun, start: startDaemonDetached}
	for _, opt := range opts {
		opt(&cfg)
	}
	binary := cfg.binary
	if binary == "" {
		located, err := locateBinary(cfg.searchRoot)
		if err != nil {
			return nil, err
		}
		binary = located
	}
	return &Client{binary: binary, run: cfg.run, start: cfg.start}, nil
}

// Path returns the resolved kron binary path the Client invokes.
func (client *Client) Path() string { return client.binary }

// Add registers job with kron, replacing any existing job of the same name. It
// returns nil only when kron confirms, via a zero exit code, that the job was
// validated and persisted. A malformed schedule yields an error matching
// ErrInvalidRequest.
func (client *Client) Add(ctx context.Context, job Job) error {
	if job.Name == "" || job.Schedule.text == "" || len(job.Command) == 0 {
		return fmt.Errorf("kron add: a job needs a name, a schedule, and a command")
	}
	args := job.addArgs()
	result, err := client.run(ctx, client.binary, args)
	if err != nil {
		return fmt.Errorf("kron add %q: %w", job.Name, err)
	}
	return classify("add", args, result)
}

// Remove deletes the named job. Removing a job that does not exist is a
// success, matching kron's idempotent behavior.
func (client *Client) Remove(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("kron remove: empty job name")
	}
	args := []string{"remove", name}
	result, err := client.run(ctx, client.binary, args)
	if err != nil {
		return fmt.Errorf("kron remove %q: %w", name, err)
	}
	return classify("remove", args, result)
}

// EnsureDaemon makes sure the kron daemon is running by starting it detached.
// It is best-effort and idempotent: kron holds a lock, so a redundant start
// exits on its own. Call it before relying on registered jobs to fire, in case
// the device's boot hook did not already start the daemon.
func (client *Client) EnsureDaemon() error {
	if err := client.start(client.binary); err != nil {
		return fmt.Errorf("kron daemon: %w", err)
	}
	return nil
}

// classify turns a kron exit code into an error per the success contract.
func classify(op string, args []string, result runResult) error {
	if result.exitCode == 0 {
		return nil
	}
	commandErr := &CommandError{
		Op:       op,
		Args:     append([]string(nil), args...),
		ExitCode: result.exitCode,
		Stderr:   result.stderr,
	}
	if result.exitCode == ExitInvalidRequest {
		commandErr.sentinel = ErrInvalidRequest
	}
	return commandErr
}

// execRun is the production runner: it invokes the real kron binary.
func execRun(ctx context.Context, binary string, args []string) (runResult, error) {
	command := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr

	err := command.Run()
	result := runResult{stdout: stdout.String(), stderr: strings.TrimSpace(stderr.String())}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.exitCode = exitErr.ExitCode()
		return result, nil
	}
	// The command could not be run at all (missing binary, cancelled context).
	return result, err
}

// startDaemonDetached launches "kron daemon" in its own session (setsid) so it
// is detached from the caller's controlling terminal and outlives the
// short-lived caller, and reaps it if it exits immediately (e.g. because
// another daemon already holds the lock).
func startDaemonDetached(binary string) error {
	command := exec.Command(binary, "daemon")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Detach the daemon's standard streams: inheriting the caller's would keep the
	// caller's stdout/stderr pipes open for the daemon's whole (long) life. /dev/null
	// is best-effort; if it cannot be opened, fall back to inherited streams.
	if devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		command.Stdin, command.Stdout, command.Stderr = devNull, devNull, devNull
		defer devNull.Close()
	}
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

// locateBinary finds kron on PATH, then under searchRoot.
func locateBinary(searchRoot string) (string, error) {
	if path, err := exec.LookPath(binaryName); err == nil {
		return path, nil
	}
	if path := findUnder(searchRoot, binaryName); path != "" {
		return path, nil
	}
	return "", ErrBinaryNotFound
}

// findUnder returns the first file named name anywhere below root, or "".
// Unreadable directories are skipped; the walk stops at the first match.
func findUnder(root, name string) string {
	var found string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting
		}
		if !entry.IsDir() && entry.Name() == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
