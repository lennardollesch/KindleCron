package main

// Schedule grammar (cron is the primary form; the "cron" keyword is optional):
//
//	<expr> | cron <expr>         5- or 6-field cron expression (see cron.go)
//	once YYYY-MM-DD HH:MM[:SS]    one absolute local datetime (no cron equivalent)
//	every <N>[s|m|h|d]           convenience: interval since last run (grid-anchored)
//	at HH:MM[,HH:MM,...]         convenience: daily at the given local wall-clock times
//
// All wall-clock math goes through the time package, which resolves the correct
// UTC offset per date, so DST transitions are handled correctly.
//
// Besides the schedule itself, the timing decisions take a registration anchor:
// the moment the job was registered (the .job file's mtime). Scheduled
// occurrences BEFORE the anchor do not count, so a freshly added job never
// "catches up" on times that passed before it existed.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type scheduleKind int

const (
	kEvery scheduleKind = iota
	kAt
	kOnce
	kCron
)

type clock struct{ h, m int }

type schedule struct {
	kind     scheduleKind
	interval time.Duration // every
	times    []clock       // at
	once     time.Time     // once (absolute, in Local)
	cron     cronExpr      // cron
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func parseInterval(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty interval")
	}
	var duration time.Duration
	if n, err := strconv.Atoi(s); err == nil { // bare number => seconds
		duration = time.Duration(n) * time.Second
	} else {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil {
			return 0, fmt.Errorf("bad interval %q", s)
		}
		switch s[len(s)-1] {
		case 's', 'S':
			duration = time.Duration(n) * time.Second
		case 'm', 'M':
			duration = time.Duration(n) * time.Minute
		case 'h', 'H':
			duration = time.Duration(n) * time.Hour
		case 'd', 'D':
			duration = time.Duration(n) * 24 * time.Hour
		default:
			return 0, fmt.Errorf("bad interval unit in %q", s)
		}
	}
	if duration <= 0 {
		return 0, fmt.Errorf("interval must be positive: %q", s)
	}
	return duration, nil
}

func parseClock(s string) (clock, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return clock{}, fmt.Errorf("bad time %q", s)
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minErr := strconv.Atoi(parts[1])
	if hourErr != nil || minErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return clock{}, fmt.Errorf("bad time %q", s)
	}
	return clock{hour, minute}, nil
}

func parseOnce(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if when, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return when, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad once datetime %q (want YYYY-MM-DD HH:MM)", s)
}

func parseSchedule(spec string) (schedule, error) {
	spec = strings.TrimSpace(spec)
	head, rest := spec, ""
	if i := strings.IndexByte(spec, ' '); i >= 0 {
		head, rest = spec[:i], strings.TrimSpace(spec[i+1:])
	}
	var parsed schedule
	switch head {
	case "every":
		duration, err := parseInterval(rest)
		if err != nil {
			return parsed, err
		}
		parsed.kind, parsed.interval = kEvery, duration
	case "at":
		if rest == "" {
			return parsed, fmt.Errorf("at: no times given")
		}
		for _, part := range strings.Split(rest, ",") {
			slot, err := parseClock(part)
			if err != nil {
				return parsed, err
			}
			parsed.times = append(parsed.times, slot)
		}
		parsed.kind = kAt
	case "once":
		when, err := parseOnce(rest)
		if err != nil {
			return parsed, err
		}
		parsed.kind, parsed.once = kOnce, when
	case "cron":
		expr, err := parseCron(rest)
		if err != nil {
			return parsed, err
		}
		parsed.kind, parsed.cron = kCron, expr
	default:
		// A bare expression with no leading keyword is treated as cron, so
		// "0 9 * * 1" is equivalent to "cron 0 9 * * 1".
		expr, err := parseCron(spec)
		if err != nil {
			return parsed, fmt.Errorf("invalid schedule %q (not every/at/once, and not a valid cron expression: %v)", spec, err)
		}
		parsed.kind, parsed.cron = kCron, expr
	}
	return parsed, nil
}

// atBounds returns the most recent occurrence (<= now) and the next (> now)
// across all listed times, in now's location (DST-correct via time.Date/AddDate).
func (sched schedule) atBounds(now time.Time) (mostRecent, next time.Time) {
	loc := now.Location()
	year, month, day := now.Date()
	for _, slot := range sched.times {
		today := time.Date(year, month, day, slot.h, slot.m, 0, 0, loc)
		var recent, upcoming time.Time
		if !today.After(now) {
			recent, upcoming = today, today.AddDate(0, 0, 1)
		} else {
			recent, upcoming = today.AddDate(0, 0, -1), today
		}
		if recent.After(mostRecent) {
			mostRecent = recent
		}
		if next.IsZero() || upcoming.Before(next) {
			next = upcoming
		}
	}
	return mostRecent, next
}

// effLast is the effective "last handled" instant: never earlier than the anchor,
// so occurrences before the job existed are ignored. For "every" jobs the daemon
// seeds a real last-run = start at startup (seedEveryJobs), so a never-run job's
// first execution lands one interval after the daemon start rather than after the
// (possibly much older) .job mtime, and the grid is a stable persisted timestamp.
func (sched schedule) effLast(last, anchor time.Time) time.Time {
	return laterOf(last, anchor)
}

// isDue reports whether the job should run at instant `at`.
func (sched schedule) isDue(last, anchor, at time.Time) bool {
	switch sched.kind {
	case kEvery:
		return !at.Before(sched.effLast(last, anchor).Add(sched.interval))
	case kAt:
		recent, _ := sched.atBounds(at)
		return sched.effLast(last, anchor).Before(recent)
	case kOnce:
		return last.Unix() <= 0 && !at.Before(sched.once)
	case kCron:
		due := sched.cron.next(sched.effLast(last, anchor))
		return !due.IsZero() && !due.After(at)
	}
	return false
}

// fireStamp returns the value to store as last-run after firing at instant `at`.
// For every/at it snaps to the scheduled grid, so the cadence neither drifts with
// wake jitter nor piles up missed runs after a long sleep. once self-removes, so
// its stamp is unused.
func (sched schedule) fireStamp(last, anchor, at time.Time) time.Time {
	switch sched.kind {
	case kEvery:
		effLast := sched.effLast(last, anchor)
		elapsed := int(at.Sub(effLast) / sched.interval) // whole intervals elapsed
		if elapsed < 1 {
			elapsed = 1
		}
		return effLast.Add(time.Duration(elapsed) * sched.interval) // most recent grid slot <= at
	case kAt:
		recent, _ := sched.atBounds(at)
		return recent
	case kCron:
		if stamp := sched.cron.prev(at); !stamp.IsZero() {
			return stamp
		}
	}
	return at
}

// nextDue returns when the job must next run; ok=false means "never again".
// Called with the real current time to program the RTC.
func (sched schedule) nextDue(last, anchor, now time.Time) (time.Time, bool) {
	switch sched.kind {
	case kEvery:
		due := sched.effLast(last, anchor).Add(sched.interval)
		if due.Before(now) { // overdue -> as soon as possible
			due = now
		}
		return due, true
	case kAt:
		recent, upcoming := sched.atBounds(now)
		if sched.effLast(last, anchor).Before(recent) { // today's occurrence still pending
			return now, true // run as soon as possible (don't wait until tomorrow)
		}
		return upcoming, true
	case kOnce:
		if last.Unix() > 0 { // already fired
			return time.Time{}, false
		}
		due := sched.once
		if due.Before(now) { // overdue (device was off) -> catch up on next wake
			due = now
		}
		return due, true
	case kCron:
		due := sched.cron.next(sched.effLast(last, anchor))
		if due.IsZero() { // unsatisfiable expression -> never again
			return time.Time{}, false
		}
		if due.Before(now) { // a slot was missed (device was off) -> catch up asap
			due = now
		}
		return due, true
	}
	return time.Time{}, false
}
