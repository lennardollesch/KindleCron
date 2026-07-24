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

// scheduleKind identifies which of the four grammar forms a schedule uses. It
// selects the timing rules applied by isDue, fireStamp and nextDue.
type scheduleKind int

const (
	kindEvery scheduleKind = iota
	kindAt
	kindOnce
	kindCron
)

// clock is a wall-clock time of day, without a date, as used by the "at" form.
type clock struct{ hour, minute int }

// schedule is a parsed schedule. Only the field belonging to kind carries a
// meaningful value; the others stay zero.
type schedule struct {
	kind     scheduleKind
	interval time.Duration // every
	times    []clock       // at
	once     time.Time     // once (absolute, in Local)
	cron     cronExpr      // cron
}

// laterOf returns whichever of the two instants is later.
func laterOf(first, second time.Time) time.Time {
	if first.After(second) {
		return first
	}
	return second
}

// parseInterval parses an "every" interval: a number with an optional s/m/h/d
// unit suffix. A bare number is read as seconds. The result must be positive.
func parseInterval(text string) (time.Duration, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, fmt.Errorf("empty interval")
	}
	var duration time.Duration
	if count, err := strconv.Atoi(text); err == nil { // bare number => seconds
		duration = time.Duration(count) * time.Second
	} else {
		count, err := strconv.Atoi(text[:len(text)-1])
		if err != nil {
			return 0, fmt.Errorf("bad interval %q", text)
		}
		switch text[len(text)-1] {
		case 's', 'S':
			duration = time.Duration(count) * time.Second
		case 'm', 'M':
			duration = time.Duration(count) * time.Minute
		case 'h', 'H':
			duration = time.Duration(count) * time.Hour
		case 'd', 'D':
			duration = time.Duration(count) * 24 * time.Hour
		default:
			return 0, fmt.Errorf("bad interval unit in %q", text)
		}
	}
	if duration <= 0 {
		return 0, fmt.Errorf("interval must be positive: %q", text)
	}
	return duration, nil
}

// parseClock parses one "HH:MM" entry of the "at" form into a time of day.
func parseClock(text string) (clock, error) {
	parts := strings.SplitN(strings.TrimSpace(text), ":", 2)
	if len(parts) != 2 {
		return clock{}, fmt.Errorf("bad time %q", text)
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return clock{}, fmt.Errorf("bad time %q", text)
	}
	return clock{hour, minute}, nil
}

// parseOnce parses the absolute datetime of the "once" form. The seconds are
// optional and the value is interpreted in the device's local time zone.
func parseOnce(text string) (time.Time, error) {
	text = strings.TrimSpace(text)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if when, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return when, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad once datetime %q (want YYYY-MM-DD HH:MM)", text)
}

// parseSchedule parses a schedule specification as stored in a job's schedule=
// line. The leading word selects the form; anything else is tried as a bare cron
// expression.
func parseSchedule(spec string) (schedule, error) {
	spec = strings.TrimSpace(spec)
	head, rest := spec, ""
	if space := strings.IndexByte(spec, ' '); space >= 0 {
		head, rest = spec[:space], strings.TrimSpace(spec[space+1:])
	}
	var parsed schedule
	switch head {
	case "every":
		duration, err := parseInterval(rest)
		if err != nil {
			return parsed, err
		}
		parsed.kind, parsed.interval = kindEvery, duration
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
		parsed.kind = kindAt
	case "once":
		when, err := parseOnce(rest)
		if err != nil {
			return parsed, err
		}
		parsed.kind, parsed.once = kindOnce, when
	case "cron":
		expr, err := parseCron(rest)
		if err != nil {
			return parsed, err
		}
		parsed.kind, parsed.cron = kindCron, expr
	default:
		// A bare expression with no leading keyword is treated as cron, so
		// "0 9 * * 1" is equivalent to "cron 0 9 * * 1".
		expr, err := parseCron(spec)
		if err != nil {
			return parsed, fmt.Errorf("invalid schedule %q (not every/at/once, and not a valid cron expression: %v)", spec, err)
		}
		parsed.kind, parsed.cron = kindCron, expr
	}
	return parsed, nil
}

// atBounds returns the most recent occurrence (<= now) and the next (> now)
// across all listed times, in now's location (DST-correct via time.Date/AddDate).
func (sched schedule) atBounds(now time.Time) (mostRecent, next time.Time) {
	location := now.Location()
	year, month, day := now.Date()
	for _, slot := range sched.times {
		today := time.Date(year, month, day, slot.hour, slot.minute, 0, 0, location)
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

// effectiveLast is the effective "last handled" instant: never earlier than the anchor,
// so occurrences before the job existed are ignored. For "every" jobs the daemon
// seeds a real last-run = start at startup (seedEveryJobs), so a never-run job's
// first execution lands one interval after the daemon start rather than after the
// (possibly much older) .job mtime, and the grid is a stable persisted timestamp.
func (sched schedule) effectiveLast(last, anchor time.Time) time.Time {
	return laterOf(last, anchor)
}

// isDue reports whether the job should run at instant `at`.
func (sched schedule) isDue(last, anchor, at time.Time) bool {
	switch sched.kind {
	case kindEvery:
		return !at.Before(sched.effectiveLast(last, anchor).Add(sched.interval))
	case kindAt:
		recent, _ := sched.atBounds(at)
		return sched.effectiveLast(last, anchor).Before(recent)
	case kindOnce:
		return last.Unix() <= 0 && !at.Before(sched.once)
	case kindCron:
		due := sched.cron.next(sched.effectiveLast(last, anchor))
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
	case kindEvery:
		effectiveLast := sched.effectiveLast(last, anchor)
		elapsed := int(at.Sub(effectiveLast) / sched.interval) // whole intervals elapsed
		if elapsed < 1 {
			elapsed = 1
		}
		return effectiveLast.Add(time.Duration(elapsed) * sched.interval) // most recent grid slot <= at
	case kindAt:
		recent, _ := sched.atBounds(at)
		return recent
	case kindCron:
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
	case kindEvery:
		due := sched.effectiveLast(last, anchor).Add(sched.interval)
		if due.Before(now) { // overdue -> as soon as possible
			due = now
		}
		return due, true
	case kindAt:
		recent, upcoming := sched.atBounds(now)
		if sched.effectiveLast(last, anchor).Before(recent) { // today's occurrence still pending
			return now, true // run as soon as possible (don't wait until tomorrow)
		}
		return upcoming, true
	case kindOnce:
		if last.Unix() > 0 { // already fired
			return time.Time{}, false
		}
		due := sched.once
		if due.Before(now) { // overdue (device was off) -> catch up on next wake
			due = now
		}
		return due, true
	case kindCron:
		due := sched.cron.next(sched.effectiveLast(last, anchor))
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
