package client

import (
	"fmt"
	"strings"
	"time"
)

// Schedule is a kron schedule specification in kron's own text syntax. Build
// one with Once, Every, At, or Cron rather than constructing the string by
// hand, so the formatting stays in one place and matches what kron parses.
type Schedule struct {
	text string
}

// String returns the schedule in kron's text form, e.g. "once 2026-07-10 09:00:00".
func (schedule Schedule) String() string { return schedule.text }

// onceLayout is kron's "once YYYY-MM-DD HH:MM:SS" timestamp format.
const onceLayout = "2006-01-02 15:04:05"

// Once builds a one-shot schedule that fires a single time at wakeTime, then
// removes itself. kron interprets the timestamp in the device's local time, so
// wakeTime should carry the intended location.
func Once(wakeTime time.Time) Schedule {
	return Schedule{"once " + wakeTime.Format(onceLayout)}
}

// Every builds a recurring schedule that fires at a fixed interval measured
// from each previous run. The interval is emitted in the coarsest unit that
// represents it exactly (days, hours, minutes, or seconds); interval must be
// positive. Sub-second precision is not supported by kron and is truncated.
func Every(interval time.Duration) Schedule {
	return Schedule{"every " + everyUnit(interval)}
}

// everyUnit renders an interval as the number-plus-unit token kron expects,
// choosing the coarsest unit that divides the interval exactly.
func everyUnit(interval time.Duration) string {
	interval = interval.Truncate(time.Second)
	switch {
	case interval%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", interval/(24*time.Hour))
	case interval%time.Hour == 0:
		return fmt.Sprintf("%dh", interval/time.Hour)
	case interval%time.Minute == 0:
		return fmt.Sprintf("%dm", interval/time.Minute)
	default:
		return fmt.Sprintf("%ds", interval/time.Second)
	}
}

// At builds a schedule that fires daily at each of the given local times
// (hour and minute only; seconds are ignored). At least one time is required.
func At(times ...time.Time) Schedule {
	clockTimes := make([]string, len(times))
	for index, moment := range times {
		clockTimes[index] = moment.Format("15:04")
	}
	return Schedule{"at " + strings.Join(clockTimes, ",")}
}

// Cron builds a schedule from a raw 5- or 6-field cron expression, e.g.
// "0 9 * * 1" (Mondays at 09:00). The expression is passed through unchanged;
// kron validates it and rejects a malformed one with ErrInvalidRequest.
func Cron(expression string) Schedule {
	return Schedule{"cron " + expression}
}
