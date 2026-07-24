package main

// Cron expressions extend the schedule grammar with calendar-based recurrence
// (weekdays, days of month, months) that the every/at/once forms cannot express.
//
// A field count of 5 or 6 is accepted and detected automatically:
//
//	min hour dom month dow            (5 fields; seconds default to 0)
//	sec min hour dom month dow        (6 fields; leading field is seconds)
//
// Each field supports the usual operators: '*' (all values), a single value,
// 'a-b' ranges, 'a-b/s' or '*/s' steps, 'v/s' (v through the field max, step s),
// and comma-separated lists of any of these. Month (jan-dec) and day-of-week
// (sun-sat) accept three-letter names; day-of-week accepts both 0 and 7 for
// Sunday.
//
// When BOTH day-of-month and day-of-week are restricted (neither is '*'), a day
// matches if EITHER field matches (the Vixie-cron convention), so "0 0 13 * 5"
// fires on every Friday and on the 13th of every month.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronExpr is a parsed cron schedule. Each field is a bitmask over its allowed
// values; a set bit means "this value matches". domStar and dowStar record
// whether the day-of-month and day-of-week fields were a literal '*', which
// selects between AND and OR matching for the two day fields (see dayMatch).
type cronExpr struct {
	sec     uint64 // bits 0-59
	min     uint64 // bits 0-59
	hour    uint32 // bits 0-23
	dom     uint32 // bits 1-31
	month   uint16 // bits 1-12
	dow     uint8  // bits 0-6 (Sunday = 0)
	domStar bool
	dowStar bool
}

// monthNames and dowNames are the three-letter symbols accepted by the month and
// day-of-week fields, matched case-insensitively.
var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// parseCron parses a 5- or 6-field cron expression. A 5-field expression is
// treated as having an implicit seconds field of 0.
func parseCron(spec string) (cronExpr, error) {
	fields := strings.Fields(spec)
	switch len(fields) {
	case 5:
		fields = append([]string{"0"}, fields...) // implicit seconds = 0
	case 6:
		// seconds given explicitly
	default:
		return cronExpr{}, fmt.Errorf("cron needs 5 or 6 fields, got %d in %q", len(fields), spec)
	}

	// field parses one column into a mask, recording the first error so the
	// caller can check once instead of after every column. star, if non-nil,
	// receives whether the column was a literal '*' (needed for the two day
	// fields).
	var parsed cronExpr
	var firstErr error
	field := func(text string, lo, hi int, names map[string]int, star *bool) uint64 {
		mask, isStar, err := parseField(text, lo, hi, names)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if star != nil {
			*star = isStar
		}
		return mask
	}

	sec := field(fields[0], 0, 59, nil, nil)
	minute := field(fields[1], 0, 59, nil, nil)
	hour := field(fields[2], 0, 23, nil, nil)
	dom := field(fields[3], 1, 31, nil, &parsed.domStar)
	month := field(fields[4], 1, 12, monthNames, nil)
	dow := field(fields[5], 0, 7, dowNames, &parsed.dowStar)
	if firstErr != nil {
		return cronExpr{}, firstErr
	}
	if dow&(1<<7) != 0 { // fold 7 (Sunday) onto 0
		dow = (dow &^ (1 << 7)) | 1
	}

	parsed.sec = sec
	parsed.min = minute
	parsed.hour = uint32(hour)
	parsed.dom = uint32(dom)
	parsed.month = uint16(month)
	parsed.dow = uint8(dow)
	return parsed, nil
}

// parseField parses one cron field into a bitmask over [lo, hi]. names maps
// case-insensitive symbolic values (e.g. "mon") to numbers, or is nil. isStar
// reports whether the field was exactly "*".
func parseField(field string, lo, hi int, names map[string]int) (mask uint64, isStar bool, err error) {
	isStar = field == "*"
	for _, term := range strings.Split(field, ",") {
		bits, termErr := parseTerm(term, lo, hi, names)
		if termErr != nil {
			return 0, false, termErr
		}
		mask |= bits
	}
	return mask, isStar, nil
}

// parseTerm parses a single comma-separated term: "*", "v", "a-b", or any of
// those with a trailing "/step".
func parseTerm(term string, lo, hi int, names map[string]int) (uint64, error) {
	step := 1
	rangePart := term
	if slash := strings.IndexByte(term, '/'); slash >= 0 {
		rangePart = term[:slash]
		parsedStep, err := strconv.Atoi(term[slash+1:])
		if err != nil || parsedStep <= 0 {
			return 0, fmt.Errorf("bad step in %q", term)
		}
		step = parsedStep
	}

	var start, end int
	switch {
	case rangePart == "*":
		start, end = lo, hi
	case strings.IndexByte(rangePart, '-') > 0:
		dash := strings.IndexByte(rangePart, '-')
		lower, lowErr := fieldValue(rangePart[:dash], names)
		upper, upErr := fieldValue(rangePart[dash+1:], names)
		if lowErr != nil || upErr != nil {
			return 0, fmt.Errorf("bad range %q", term)
		}
		start, end = lower, upper
	default:
		value, err := fieldValue(rangePart, names)
		if err != nil {
			return 0, fmt.Errorf("bad value %q", term)
		}
		start = value
		if strings.ContainsRune(term, '/') {
			end = hi // "v/step" means v through the field maximum
		} else {
			end = value
		}
	}

	if start < lo || end > hi || start > end {
		return 0, fmt.Errorf("value out of range in %q (allowed %d-%d)", term, lo, hi)
	}
	var mask uint64
	for value := start; value <= end; value += step {
		mask |= 1 << uint(value)
	}
	return mask, nil
}

// fieldValue resolves one numeric or symbolic value of a cron field, e.g. "3"
// or "mon". names is the field's symbol table, or nil if it has none.
func fieldValue(text string, names map[string]int) (int, error) {
	text = strings.TrimSpace(text)
	if names != nil {
		if value, ok := names[strings.ToLower(text)]; ok {
			return value, nil
		}
	}
	return strconv.Atoi(text)
}

// dayMatch reports whether a given day-of-month and weekday satisfy the two day
// fields. If both are restricted, a match in EITHER field is enough (Vixie
// convention); otherwise the '*' field is unrestricted and both must match.
func (expr cronExpr) dayMatch(day, weekday int) bool {
	domOK := expr.dom&(1<<uint(day)) != 0
	dowOK := expr.dow&(1<<uint(weekday)) != 0
	if !expr.domStar && !expr.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// searchBound limits how far next/prev scan before declaring a match
// unreachable (e.g. "0 0 30 2 *", February 30th). A valid expression always
// matches within a few years; this only guards genuinely impossible ones.
const searchBound = 5

// next returns the earliest matching instant strictly after the given time, at
// one-second resolution. A zero time means no match exists within searchBound
// years (the expression is unsatisfiable). All arithmetic stays in the input's
// location, so the time package resolves DST and month lengths.
func (expr cronExpr) next(after time.Time) time.Time {
	candidate := after.Truncate(time.Second).Add(time.Second)
	limit := candidate.Year() + searchBound
	location := candidate.Location()
	for {
		if candidate.Year() > limit {
			return time.Time{}
		}
		// Each case skips the candidate forward to the start of the next unit of
		// the coarsest field that does not match, so unmatched years and months
		// are stepped over instead of scanned second by second.
		year, month, day := candidate.Date()
		hour, minute, _ := candidate.Clock()
		switch {
		case expr.month&(1<<uint(month)) == 0:
			candidate = time.Date(year, month, 1, 0, 0, 0, 0, location).AddDate(0, 1, 0)
		case !expr.dayMatch(day, int(candidate.Weekday())):
			candidate = time.Date(year, month, day, 0, 0, 0, 0, location).AddDate(0, 0, 1)
		case expr.hour&(1<<uint(hour)) == 0:
			candidate = time.Date(year, month, day, hour, 0, 0, 0, location).Add(time.Hour)
		case expr.min&(1<<uint(minute)) == 0:
			candidate = time.Date(year, month, day, hour, minute, 0, 0, location).Add(time.Minute)
		case expr.sec&(1<<uint(candidate.Second())) == 0:
			candidate = candidate.Add(time.Second)
		default:
			return candidate
		}
	}
}

// prev returns the latest matching instant at or before the given time, at
// one-second resolution. It mirrors next, stepping backward to the last second
// of the preceding field when a field does not match. A zero time means no
// match exists within searchBound years.
func (expr cronExpr) prev(before time.Time) time.Time {
	candidate := before.Truncate(time.Second)
	limit := candidate.Year() - searchBound
	location := candidate.Location()
	for {
		if candidate.Year() < limit {
			return time.Time{}
		}
		// Mirrors next: an unmatched field rewinds the candidate to the last
		// second before that field's current unit began.
		year, month, day := candidate.Date()
		hour, minute, _ := candidate.Clock()
		switch {
		case expr.month&(1<<uint(month)) == 0:
			candidate = time.Date(year, month, 1, 0, 0, 0, 0, location).Add(-time.Second)
		case !expr.dayMatch(day, int(candidate.Weekday())):
			candidate = time.Date(year, month, day, 0, 0, 0, 0, location).Add(-time.Second)
		case expr.hour&(1<<uint(hour)) == 0:
			candidate = time.Date(year, month, day, hour, 0, 0, 0, location).Add(-time.Second)
		case expr.min&(1<<uint(minute)) == 0:
			candidate = time.Date(year, month, day, hour, minute, 0, 0, location).Add(-time.Second)
		case expr.sec&(1<<uint(candidate.Second())) == 0:
			candidate = candidate.Add(-time.Second)
		default:
			return candidate
		}
	}
}
