<p align="center">
  <h1 align="center">KindleCron (<code>kron</code>)</h1>
</p>

<p align="center">
  Cron-like task scheduling for jailbroken Kindles that survives deep sleep.
</p>

<p align="center">
  <a href="https://github.com/lennardollesch/KindleCron/releases/latest"><img src="https://img.shields.io/github/v/release/lennardollesch/KindleCron"></a> <a href="https://github.com/lennardollesch/KindleCron"><img src="https://img.shields.io/github/stars/lennardollesch/KindleCron"></a>
</p>

---

## About

KindleCron runs scheduled jobs on a jailbroken Kindle reliably and to the second, while letting the device sleep between them. It is a single static binary with nothing to install on the device.

Ordinary `cron` is unreliable here. To save power, the Kindle suspends (CPU off) after a few minutes of inactivity, and a suspended device cannot run `crond`. The common workaround, holding the device in its `active` state, drains the battery.

KindleCron takes the opposite approach. During the brief `readyToSuspend` window just before the device sleeps, it programs an RTC wake-up for the next job's due time; set this late in the cycle, the value is not overwritten by powerd. The Kindle then suspends normally and conserves power. When the job is due, the RTC fires, the device wakes into `screenSaver` state, becomes responsive and KindleCron runs the job.

That underlying trick is not new. It has been known in the Kindle community for years (see [Credits](#credits)) and various scripts implement it ad hoc for one purpose each. What KindleCron adds is the packaging: a single, self-contained binary that turns the trick into a general-purpose scheduler with a cron grammar, persistent job state, keep-awake handling, per-job timeouts, logging and a defined exit-code contract. The power-state handling lives in one place and is maintained there, so any project that needs work to happen on a sleeping Kindle can schedule it in one command instead of reimplementing the powerd procedure itself.

> [!WARNING]
> Jobs scheduled more often than roughly every 3 minutes keep the device from suspending at all, draining the battery noticeably faster.

> [!NOTE]
> Tested on Kindle Basic 10th gen (KT4, firmware 5.13.6) and Paperwhite 4 (PW4, firmware 5.17.1.0.3). Other jailbroken models with the same `powerd` service are expected to work; reports are welcome.

## Installation

1. Download the [latest release](https://github.com/lennardollesch/KindleCron/releases/latest) to your computer
2. Unzip it
3. Connect the Kindle over USB
4. Copy the `extensions` and `kron` folders to the root of the Kindle drive (`/mnt/us`)
5. Run **Setup** once (KUAL: *kron -> Setup*, or a terminal: `/mnt/us/kron/kron setup`)


> [!NOTE]
> If you don't control kron through KUAL, you can leave out the `extensions` folder.  
**Setup** is optional too: it symlinks the binary into `/usr/bin`, so `kron` can be called from anywhere. Some programs may expect it, otherwise use the full path (`/mnt/us/kron/kron`).

## Usage

```
kron [global flags] <command> [args]

Commands:
  daemon                              run the scheduler (default; start this at boot)
  stop                                tell a running daemon to shut down
  kill-jobs [NAME]                    kill orphaned job process group(s); all, or just NAME
  add [-timeout DUR] NAME SCHEDULE COMMAND...
                                      register or replace a job; NAME is a plain name
                                      (no '/' or leading '.')
  remove NAME                         delete a job and its state
  enable NAME                         re-enable a disabled job (resumes scheduling)
  disable NAME                        stop scheduling a job without deleting it
  purge                               delete ALL jobs and their state (-y to skip prompt)
  clear-logs                          empty kron.log and all per-job logs (-y to skip prompt)
  list                                show all jobs, when each next runs, and any timeout
  setup                               symlink kron into /usr/bin so a bare `kron` works everywhere
  unlink                              remove the /usr/bin/kron symlink that setup created
  version | help

Global flags:
  -dir PATH        data directory for jobs/state/log
                   (default: the binary's own directory; overrides $KRON_DIR)
  -logmax KB       cap kron.log at KB kilobytes, dropping oldest lines
                   (0 = unlimited; default 256)
  -keepawake DUR   keep the device awake (no suspend) when the next job is due within
                   DUR (default 3m); 'off' to always suspend regardless of interval
  -wakelead DUR    wake this long BEFORE a job is due as a safety margin (default 15s;
                   0 disables it)
  -jobtimeout DUR  global default run-time limit per job (default 10m); a job exceeding
                   its effective limit is killed. Override per job with add -timeout
  -version         print version
  -eips            also show the result on the Kindle screen (centred, lower area) via
                   eips. Meant for KUAL, where there is no terminal. Supported by
                   daemon, stop, version, purge, kill-jobs, clear-logs, setup and unlink.
  -help            show help
```

Global flags must come **before** the subcommand (`kron -keepawake 5m daemon`), or,
for commands that take no positional arguments (`daemon`, `list`, `stop`, `version`,
`setup`, `unlink`), in either position.

> [!NOTE]
> The `-eips` flag mirrors a command's result onto the device screen (centred, lower
> area) using Kindle's `eips` tool, so you get a visible readout from KUAL where
> there is no terminal. It is supported by `daemon`, `stop`, `version`, `purge`,
> `kill-jobs`, `clear-logs`, `setup` and `unlink` (e.g. `kron purge -y -eips`). The on-screen rendering
> is handled by the shared
> [kindle-utils](https://github.com/lennardollesch/kindle-utils) `eips` package.

### Exit codes

kron's exit code is the success signal, so another program (a launcher, an
installer, [Scopae](https://github.com/lennardollesch/Scopae)) can drive it without parsing human-readable stdout:

| Code | Meaning |
| --- | --- |
| `0` | success - for `add`, the schedule was validated **and** the job written; `remove` is idempotent (removing a missing job still succeeds) |
| `2` | invalid request: bad arguments or an unparseable schedule |
| `1` | any other failure (I/O, permissions, ...) |

stdout stays human-readable and may change wording (e.g. the keep-awake note),
so callers should rely on the exit code.

>[!NOTE]
For Go programs, the official client in [`client/`](client/) wraps these
invocations (`client.New`, `Add`, `Remove`) and maps the exit codes to typed
errors, so a consumer never parses stdout.

### Schedule formats

A **cron expression is the primary form**, and the `cron` keyword is optional, so a
bare expression is the preferred way to write a schedule. `once` is the only form
with no cron equivalent; `every` and `at` are convenience shorthands for cadences
that cron already covers.

```
<cron expr>                 5- or 6-field cron, e.g. 0 9 * * 1 (Mondays 09:00)
once YYYY-MM-DD HH:MM[:SS]  one-shot; runs once (catches up if missed) then self-removes
every <N>[s|m|h|d]          convenience: fixed interval since last run, e.g. every 30m
at HH:MM[,HH:MM,...]        convenience: daily at local times (same as cron M H * * *)
```

```sh
kron add weekly   "0 9 * * 1"             /mnt/us/app/weekly.sh
kron add poll     "*/30 * * * * *"        /mnt/us/app/poll.sh
kron add reminder "once 2026-06-15 14:30" /mnt/us/app/notify.sh
```

`every` (a fixed interval since the last run, and the only form
that also does arbitrary sub-minute intervals like `90s`) and `at` (daily
wall-clock times, the same as `cron M H * * *`) are sugars you can use when they
read more clearly.

### Cron expressions

A `cron` schedule takes a standard cron expression. The `cron` keyword is
optional: a bare expression is treated as cron, so `kron add j "0 9 * * 1" ...`
is the same as `kron add j "cron 0 9 * * 1" ...`. The field count is detected
automatically: **5 fields** is the classic form, **6 fields** adds a leading
seconds field.

```
5-field:      min  hour  dom  month  dow
6-field:  sec min  hour  dom  month  dow

  sec    second        0-59   (6-field form only)
  min    minute        0-59
  hour   hour          0-23
  dom    day of month  1-31
  month  month         1-12 or jan-dec
  dow    day of week   0-7 or sun-sat (0 and 7 are both Sunday)
```

Each field accepts `*` (all values), a single value, `a-b` ranges, `*/s` or
`a-b/s` steps, `v/s` (from `v` to the field maximum, step `s`), and comma lists
of any of these. Month and day-of-week also accept three-letter names.

```sh
kron add j "30 7 * * 1-5"      ...   # weekdays at 07:30
kron add j "0 0 1 * *"         ...   # midnight on the 1st of every month
kron add j "0 */2 * * *"       ...   # every 2 hours on the hour
kron add j "*/30 * * * * *"    ...   # every 30 seconds (6-field)
kron add j "0 9 * * mon,thu"   ...   # Mondays and Thursdays at 09:00
```

When **both** the day-of-month and day-of-week fields are restricted (neither is
`*`), a day matches if **either** field matches, following the Vixie-cron
convention. So `0 0 13 * 5` fires on every Friday **and** on the 13th of every
month. Wall-clock times that do not exist on a DST spring-forward day are skipped
for that day.

### Per-job timeout

A job may declare its own run-time limit, which overrides the global `-jobtimeout`
default (in either direction). A job that exceeds its effective limit is killed
(its whole process group), so a hung job can never pin the device awake.

```sh
kron add sync -timeout 2m "every 1h" /mnt/us/app/sync.sh
```

The limit is shown by `kron list`, and stored in the job file as `timeout=2m`.

## powerd internals

KindleCron drives the device through Kindle's `powerd` service. The states and
properties below were reverse-engineered (see [Credits](#credits)) and are the
behavior KindleCron relies on. Timings are approximate and vary by model.

### Power states

| State | Power draw | Responsive | Entered after idle |
| --- | --- | --- | --- |
| `active` | full | yes | on wake or interaction |
| `screenSaver` | display off | yes | ~10 min |
| `readyToSuspend` | display off | yes (~50 s window) | ~11 min |
| `suspend` | CPU frozen | no (wakes on RTC or power button) | ~12 min |

### Properties

All are write-only integers, set with `lipc-set-prop -i com.lab126.powerd <property> <value>`.

| Property | Behavior |
| --- | --- |
| `rtcWakeup` | Must be set during the ~50 s `readyToSuspend` window, or powerd overwrites it. Schedules an RTC wake that survives `suspend` and returns the device to `screenSaver`. This is KindleCron's core mechanism. |
| `abortSuspend` | `abortSuspend 1` during `readyToSuspend` restarts the suspend countdown without leaving the state, keeping the device up for an imminent job. |
| `deferSuspend` | Not used by KindleCron; set during `readyToSuspend`, defers the suspend and sends the device back to `active`. |
| `suspendGrace` | Not used by KindleCron; behavior unconfirmed. |
| `addSuspendLevels` | Not used by KindleCron; behavior unconfirmed. |

## Credits

- [**yparitcher**](https://github.com/yparitcher) - the [RTC wakeup idea](https://www.mobileread.com/forums/showthread.php?t=322900)
- [**KindleModding**](https://github.com/KindleModding) - the [powerd documentation](https://kindlemodding.org/kindle-apps-and-services/com.lab126.powerd.html)

## License

Copyright (C) 2026 Lennard Ollesch

KindleCron is free software, licensed under the GNU Affero General Public License
v3.0 or later (`AGPL-3.0-or-later`). It comes with no warranty. You may use, study,
share, and modify it under the terms of the license; if you distribute it or a
modified version, you must pass on the same freedoms and make the source available.
See the [LICENSE](LICENSE) file for the full text.
