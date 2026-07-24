# KindleCron Go client

The official Go client for driving the `kron` binary from another program (for
example [Scopae](https://github.com/lennardollesch)).

```sh
go get github.com/lennardollesch/KindleCron/client
```

This is a separate Go module inside the KindleCron repository, so it is versioned
on its own with `client/vX.Y.Z` tags and moves independently of the `kron`
binary's releases.

## Example

```go
package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/lennardollesch/KindleCron/client"
)

func main() {
	kron, err := client.New() // discover on PATH, then /mnt/us; or client.WithBinary(path)
	if err != nil {
		log.Fatal(err)
	}

	err = kron.Add(context.Background(), client.Job{
		Name:     "backup",
		Schedule: client.Every(6 * time.Hour),
		Command:  []string{"/mnt/us/app/backup.sh"},
	})
	switch {
	case errors.Is(err, client.ErrInvalidRequest):
		log.Fatal("bad schedule or arguments (a bug to fix)")
	case err != nil:
		log.Fatalf("kron failed: %v", err)
	}
	// exit 0: the job was validated and persisted.

	// Optional: make sure the daemon is up, in case the device's boot hook did
	// not start it. Idempotent, a redundant start exits on the singleton lock.
	if err := kron.EnsureDaemon(); err != nil {
		log.Printf("could not start the kron daemon: %v", err)
	}
}
```

## Schedules

Build the `Schedule` value with a constructor rather than a raw string, so the
formatting stays in sync with what `kron` parses:

| Constructor | Produces |
| --- | --- |
| `client.Once(t)` | `once 2026-07-10 09:00:00` (one-shot, then self-removes) |
| `client.Every(d)` | `every 6h` (fixed interval since the last run) |
| `client.At(t, ...)` | `at 07:00,19:00` (daily at local times) |
| `client.Cron(expr)` | `cron 0 9 * * 1` (5- or 6-field cron expression) |

## Options

| Option | Effect |
| --- | --- |
| `client.WithBinary(path)` | use an explicit kron path and skip discovery |
| `client.WithSearchRoot(dir)` | search this directory instead of `/mnt/us` when kron is not on PATH |

Discovery consults `PATH` first, where `kron setup` installs its symlink, and
falls back to a walk of the search root. If neither finds it, `New` returns
`ErrBinaryNotFound`.

## Contract

| kron exit | Meaning | Surfaced as |
| --- | --- | --- |
| `0` | request validated **and** applied | `nil` |
| `2` | invalid schedule or arguments | `ErrInvalidRequest` (wrapped in `*CommandError`) |
| other | any other failure (I/O, permissions, ...) | `*CommandError` |

`Add` is validate-then-persist and `Remove` is idempotent, so a `nil` return is a
firm confirmation.