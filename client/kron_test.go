package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunner scripts kron invocations: it records the argument vectors it is
// called with and replays a queued result for each call.
type fakeRunner struct {
	calls   [][]string
	replies []runResult
	runErr  error
}

func (fake *fakeRunner) run(_ context.Context, _ string, args []string) (runResult, error) {
	fake.calls = append(fake.calls, args)
	if fake.runErr != nil {
		return runResult{}, fake.runErr
	}
	if len(fake.replies) == 0 {
		return runResult{}, nil // default: success
	}
	reply := fake.replies[0]
	fake.replies = fake.replies[1:]
	return reply, nil
}

// newTestClient builds a Client wired to a fake runner and a no-op daemon
// starter, skipping binary discovery.
func newTestClient(t *testing.T, fake *fakeRunner) *Client {
	t.Helper()
	client, err := New(
		WithBinary("kron"),
		withRunner(fake.run),
		withStarter(func(string) error { return nil }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestAdd_BuildsArgumentsAndAcceptsZeroExit(t *testing.T) {
	fake := &fakeRunner{}
	client := newTestClient(t, fake)

	job := Job{
		Name:     "Scopae_Schedule",
		Schedule: Once(time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)),
		Command:  []string{"/mnt/us/scopae/scopae"},
	}
	if err := client.Add(context.Background(), job); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	want := "add Scopae_Schedule once 2026-07-10 09:00:00 /mnt/us/scopae/scopae"
	if got := strings.Join(fake.calls[0], " "); got != want {
		t.Errorf("add args = %q, want %q", got, want)
	}
}

func TestAdd_IncludesTimeoutFlagWhenSet(t *testing.T) {
	fake := &fakeRunner{}
	client := newTestClient(t, fake)

	job := Job{
		Name:     "job",
		Schedule: Every(30 * time.Minute),
		Command:  []string{"/bin/true"},
		Timeout:  5 * time.Minute,
	}
	if err := client.Add(context.Background(), job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got := strings.Join(fake.calls[0], " ")
	want := "add -timeout 5m0s job every 30m /bin/true"
	if got != want {
		t.Errorf("add args = %q, want %q", got, want)
	}
}

func TestAdd_InvalidRequestExitMapsToSentinel(t *testing.T) {
	fake := &fakeRunner{replies: []runResult{
		{exitCode: ExitInvalidRequest, stderr: "invalid schedule: bad spec"},
	}}
	client := newTestClient(t, fake)

	err := client.Add(context.Background(), Job{
		Name:     "job",
		Schedule: Cron("nonsense"),
		Command:  []string{"/bin/true"},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Add error = %v, want it to match ErrInvalidRequest", err)
	}

	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("Add error = %v, want a *CommandError", err)
	}
	if commandErr.ExitCode != ExitInvalidRequest || commandErr.Stderr != "invalid schedule: bad spec" {
		t.Errorf("CommandError = %+v, missing exit code or stderr", commandErr)
	}
}

func TestAdd_OtherNonZeroExitIsGenericCommandError(t *testing.T) {
	fake := &fakeRunner{replies: []runResult{{exitCode: 1, stderr: "write failed"}}}
	client := newTestClient(t, fake)

	err := client.Add(context.Background(), Job{
		Name:     "job",
		Schedule: Every(time.Hour),
		Command:  []string{"/bin/true"},
	})
	if errors.Is(err, ErrInvalidRequest) {
		t.Error("a generic failure must not match ErrInvalidRequest")
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) || commandErr.ExitCode != 1 {
		t.Fatalf("Add error = %v, want a *CommandError with exit 1", err)
	}
}

func TestAdd_RejectsIncompleteJobWithoutCallingKron(t *testing.T) {
	fake := &fakeRunner{}
	client := newTestClient(t, fake)

	cases := map[string]Job{
		"no name":     {Schedule: Every(time.Hour), Command: []string{"x"}},
		"no schedule": {Name: "j", Command: []string{"x"}},
		"no command":  {Name: "j", Schedule: Every(time.Hour)},
	}
	for name, job := range cases {
		t.Run(name, func(t *testing.T) {
			if err := client.Add(context.Background(), job); err == nil {
				t.Error("expected an error for an incomplete job")
			}
		})
	}
	if len(fake.calls) != 0 {
		t.Errorf("kron was invoked %d time(s) for invalid jobs, want 0", len(fake.calls))
	}
}

func TestAdd_RunnerFailurePropagates(t *testing.T) {
	fake := &fakeRunner{runErr: errors.New("exec: not started")}
	client := newTestClient(t, fake)

	err := client.Add(context.Background(), Job{
		Name:     "job",
		Schedule: Every(time.Hour),
		Command:  []string{"/bin/true"},
	})
	if err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("Add error = %v, want it to wrap the runner failure", err)
	}
}

func TestRemove_BuildsArgumentsAndSucceedsOnZeroExit(t *testing.T) {
	fake := &fakeRunner{}
	client := newTestClient(t, fake)

	if err := client.Remove(context.Background(), "Scopae_Schedule"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := strings.Join(fake.calls[0], " "); got != "remove Scopae_Schedule" {
		t.Errorf("remove args = %q, want %q", got, "remove Scopae_Schedule")
	}
}

func TestRemove_EmptyNameIsRejected(t *testing.T) {
	fake := &fakeRunner{}
	client := newTestClient(t, fake)

	if err := client.Remove(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty job name")
	}
	if len(fake.calls) != 0 {
		t.Error("kron must not be invoked for an empty name")
	}
}

func TestEnsureDaemon_InvokesStarter(t *testing.T) {
	started := ""
	client, err := New(
		WithBinary("/mnt/us/kron/kron"),
		withRunner((&fakeRunner{}).run),
		withStarter(func(binary string) error { started = binary; return nil }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := client.EnsureDaemon(); err != nil {
		t.Fatalf("EnsureDaemon: %v", err)
	}
	if started != "/mnt/us/kron/kron" {
		t.Errorf("daemon started with %q, want the resolved binary path", started)
	}
}

func TestEnsureDaemon_StarterErrorIsWrapped(t *testing.T) {
	client, err := New(
		WithBinary("kron"),
		withRunner((&fakeRunner{}).run),
		withStarter(func(string) error { return errors.New("permission denied") }),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.EnsureDaemon(); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("EnsureDaemon error = %v, want it to wrap the starter failure", err)
	}
}

func TestNew_ExplicitBinarySkipsDiscovery(t *testing.T) {
	client, err := New(WithBinary("/custom/kron"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.Path() != "/custom/kron" {
		t.Errorf("Path = %q, want the explicit binary", client.Path())
	}
}

func TestNew_FindsBinaryUnderSearchRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(nested, binaryName)
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	client, err := New(WithSearchRoot(root))
	if err != nil {
		t.Fatalf("New with search root: %v", err)
	}
	if client.Path() != binary {
		t.Errorf("Path = %q, want %q", client.Path(), binary)
	}
}

func TestNew_ReportsBinaryNotFound(t *testing.T) {
	_, err := New(WithSearchRoot(t.TempDir()))
	if !errors.Is(err, ErrBinaryNotFound) {
		t.Errorf("New error = %v, want ErrBinaryNotFound", err)
	}
}
