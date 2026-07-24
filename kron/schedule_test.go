package main

import (
	"testing"
	"time"
)

// With the seeding model, a never-run "every" job gets last-run = start written at
// daemon startup. After that, effectiveLast = laterOf(last, anchor) = the seeded start,
// so the first run is start+interval. These tests exercise the scheduling math
// directly with an explicit seeded last-run.

func TestEveryFirstRunAnchoredToSeed(t *testing.T) {
	start := time.Date(2026, 6, 9, 19, 11, 20, 0, time.Local)
	sched := schedule{kind: kindEvery, interval: 6 * time.Minute}

	last := start
	anchor := start.Add(-24 * time.Second)

	nextRun, ok := sched.nextDue(last, anchor, start)
	if !ok {
		t.Fatal("nextDue not ok")
	}
	want := start.Add(6 * time.Minute)
	if !nextRun.Equal(want) {
		t.Fatalf("first run = %s, want %s", nextRun.Format("15:04:05"), want.Format("15:04:05"))
	}
}

func TestEveryUsesLastOnceRun(t *testing.T) {
	start := time.Date(2026, 6, 9, 19, 0, 0, 0, time.Local)
	sched := schedule{kind: kindEvery, interval: 6 * time.Minute}
	anchor := start.Add(-1 * time.Hour)
	last := start.Add(10 * time.Minute)

	nextRun, ok := sched.nextDue(last, anchor, last)
	if !ok {
		t.Fatal("nextDue not ok")
	}
	want := last.Add(6 * time.Minute)
	if !nextRun.Equal(want) {
		t.Fatalf("next run = %s, want %s", nextRun.Format("15:04:05"), want.Format("15:04:05"))
	}
}

func TestEveryNotDueBeforeInterval(t *testing.T) {
	start := time.Date(2026, 6, 9, 12, 0, 0, 0, time.Local)
	sched := schedule{kind: kindEvery, interval: 5 * time.Minute}
	anchor := start.Add(-2 * time.Hour)
	last := start

	if sched.isDue(last, anchor, start.Add(4*time.Minute)) {
		t.Fatal("job due before one interval elapsed since seed")
	}
	if !sched.isDue(last, anchor, start.Add(5*time.Minute)) {
		t.Fatal("job not due after one interval since seed")
	}
}

// TestEveryGridStableAfterEarlyWake covers the wakeLead margin: the RTC wakes a
// few seconds early, the job is not yet due, and nextDue must still point at the
// grid slot (start+interval), not at interval-minus-elapsed.
func TestEveryGridStableAfterEarlyWake(t *testing.T) {
	start := time.Date(2026, 6, 10, 15, 54, 51, 0, time.Local)
	sched := schedule{kind: kindEvery, interval: 10 * time.Minute}
	anchor := start.Add(-5 * time.Minute)
	last := start

	firstSlot := start.Add(10 * time.Minute)

	earlyWake := firstSlot.Add(-10 * time.Second)
	if sched.isDue(last, anchor, earlyWake) {
		t.Fatal("job reported due 10s before its slot")
	}
	nextRun, _ := sched.nextDue(last, anchor, earlyWake)
	if !nextRun.Equal(firstSlot) {
		t.Fatalf("nextDue after early wake = %s, want %s", nextRun.Format("15:04:05"), firstSlot.Format("15:04:05"))
	}
	if gap := nextRun.Sub(earlyWake); gap != 10*time.Second {
		t.Fatalf("time-to-next = %s, want 10s (grid must not drift by elapsed-before-suspend)", gap)
	}
}
