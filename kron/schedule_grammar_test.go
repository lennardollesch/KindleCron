package main

import (
	"testing"
	"time"
)

func TestParseScheduleEvery(t *testing.T) {
	cases := []struct {
		spec     string
		interval time.Duration
	}{
		{"every 30m", 30 * time.Minute},
		{"every 2h", 2 * time.Hour},
		{"every 1d", 24 * time.Hour},
		{"every 90s", 90 * time.Second},
		{"every 45", 45 * time.Second}, // bare number = seconds
	}
	for _, testCase := range cases {
		got, err := parseSchedule(testCase.spec)
		if err != nil {
			t.Fatalf("parseSchedule(%q): %v", testCase.spec, err)
		}
		if got.kind != kindEvery || got.interval != testCase.interval {
			t.Fatalf("%q: kind=%d interval=%s, want every %s", testCase.spec, got.kind, got.interval, testCase.interval)
		}
	}
}

func TestParseScheduleAtAndOnce(t *testing.T) {
	at, err := parseSchedule("at 07:00,19:30")
	if err != nil {
		t.Fatal(err)
	}
	if at.kind != kindAt || len(at.times) != 2 {
		t.Fatalf("at parse = %+v", at)
	}
	if at.times[0] != (clock{7, 0}) || at.times[1] != (clock{19, 30}) {
		t.Fatalf("at times = %+v", at.times)
	}

	once, err := parseSchedule("once 2026-06-15 14:30")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 15, 14, 30, 0, 0, time.Local)
	if once.kind != kindOnce || !once.once.Equal(want) {
		t.Fatalf("once = %+v, want %s", once, want)
	}
}

func TestParseScheduleErrors(t *testing.T) {
	for _, spec := range []string{
		"", "every", "every 0", "every -5", "every 10x",
		"at", "at 24:00", "at 07:60", "at 7",
		"once", "once not-a-date", "bogus 5",
	} {
		if _, err := parseSchedule(spec); err == nil {
			t.Fatalf("parseSchedule(%q) = nil error, want error", spec)
		}
	}
}

func TestAtBounds(t *testing.T) {
	sched := schedule{kind: kindAt, times: []clock{{7, 0}, {19, 0}}}
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)

	recent, next := sched.atBounds(now)
	wantRecent := time.Date(2026, 6, 15, 7, 0, 0, 0, time.Local)
	wantNext := time.Date(2026, 6, 15, 19, 0, 0, 0, time.Local)
	if !recent.Equal(wantRecent) {
		t.Fatalf("mostRecent = %s, want %s", recent, wantRecent)
	}
	if !next.Equal(wantNext) {
		t.Fatalf("next = %s, want %s", next, wantNext)
	}
}

func TestAtDueAndNextDue(t *testing.T) {
	sched := schedule{kind: kindAt, times: []clock{{7, 0}, {19, 0}}}
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)
	anchor := now.Add(-48 * time.Hour)

	// Today's 07:00 has passed and was never handled -> due now.
	if !sched.isDue(anchor, anchor, now) {
		t.Fatal("at job should be due when an occurrence passed unhandled")
	}
	due, ok := sched.nextDue(anchor, anchor, now)
	if !ok || !due.Equal(now) {
		t.Fatalf("nextDue = %s ok=%v, want now (run asap)", due, ok)
	}

	// After handling today's 07:00, the next occurrence is today's 19:00.
	handled := time.Date(2026, 6, 15, 7, 0, 0, 0, time.Local)
	if sched.isDue(handled, anchor, now) {
		t.Fatal("at job should not be due right after handling 07:00")
	}
	nextDue, _ := sched.nextDue(handled, anchor, now)
	want := time.Date(2026, 6, 15, 19, 0, 0, 0, time.Local)
	if !nextDue.Equal(want) {
		t.Fatalf("nextDue after 07:00 = %s, want 19:00", nextDue)
	}

	// fireStamp snaps to the most recent occurrence.
	if stamp := sched.fireStamp(anchor, anchor, now); !stamp.Equal(handled) {
		t.Fatalf("fireStamp = %s, want %s", stamp, handled)
	}
}

func TestOnceNextDue(t *testing.T) {
	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.Local)
	sched := schedule{kind: kindOnce, once: future}
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)

	due, ok := sched.nextDue(time.Unix(0, 0), now, now)
	if !ok || !due.Equal(future) {
		t.Fatalf("once nextDue = %s ok=%v, want %s", due, ok, future)
	}

	// Already fired (last-run set) -> never again.
	if _, ok := sched.nextDue(now, now, now); ok {
		t.Fatal("a fired once job must report never-again")
	}

	// Overdue once (device was off) catches up on the next evaluation.
	overdue := schedule{kind: kindOnce, once: now.Add(-time.Hour)}
	caught, ok := overdue.nextDue(time.Unix(0, 0), now.Add(-2*time.Hour), now)
	if !ok || !caught.Equal(now) {
		t.Fatalf("overdue once = %s, want now", caught)
	}
}
