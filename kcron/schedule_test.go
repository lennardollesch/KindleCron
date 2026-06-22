package main

import (
	"testing"
	"time"
)

// With the seeding model, a never-run "every" job gets last-run = start written at
// daemon startup. After that, effLast = laterOf(last, anchor) = the seeded start,
// so the first run is start+interval. These tests exercise the scheduling math
// directly with an explicit seeded last-run.

func TestEveryFirstRunAnchoredToSeed(t *testing.T) {
	start := time.Date(2026, 6, 9, 19, 11, 20, 0, time.Local)
	s := schedule{kind: kEvery, interval: 6 * time.Minute}

	last := start
	anchor := start.Add(-24 * time.Second)

	nd, ok := s.nextDue(last, anchor, start)
	if !ok {
		t.Fatal("nextDue not ok")
	}
	want := start.Add(6 * time.Minute)
	if !nd.Equal(want) {
		t.Fatalf("first run = %s, want %s", nd.Format("15:04:05"), want.Format("15:04:05"))
	}
}

func TestEveryUsesLastOnceRun(t *testing.T) {
	start := time.Date(2026, 6, 9, 19, 0, 0, 0, time.Local)
	s := schedule{kind: kEvery, interval: 6 * time.Minute}
	anchor := start.Add(-1 * time.Hour)
	last := start.Add(10 * time.Minute)

	nd, ok := s.nextDue(last, anchor, last)
	if !ok {
		t.Fatal("nextDue not ok")
	}
	want := last.Add(6 * time.Minute)
	if !nd.Equal(want) {
		t.Fatalf("next run = %s, want %s", nd.Format("15:04:05"), want.Format("15:04:05"))
	}
}

func TestEveryNotDueBeforeInterval(t *testing.T) {
	start := time.Date(2026, 6, 9, 12, 0, 0, 0, time.Local)
	s := schedule{kind: kEvery, interval: 5 * time.Minute}
	anchor := start.Add(-2 * time.Hour)
	last := start

	if s.isDue(last, anchor, start.Add(4*time.Minute)) {
		t.Fatal("job due before one interval elapsed since seed")
	}
	if !s.isDue(last, anchor, start.Add(5*time.Minute)) {
		t.Fatal("job not due after one interval since seed")
	}
}

// TestEveryGridStableAfterEarlyWake reproduces the 'every 10m' log bug: the RTC
// wakes a few seconds early, the job is not yet due, and nextDue must still point
// at the correct grid slot (start+interval) - NOT interval-minus-elapsed.
func TestEveryGridStableAfterEarlyWake(t *testing.T) {
	start := time.Date(2026, 6, 10, 15, 54, 51, 0, time.Local)
	s := schedule{kind: kEvery, interval: 10 * time.Minute}
	anchor := start.Add(-5 * time.Minute)
	last := start

	firstSlot := start.Add(10 * time.Minute)

	earlyWake := firstSlot.Add(-10 * time.Second)
	if s.isDue(last, anchor, earlyWake) {
		t.Fatal("job reported due 10s before its slot")
	}
	nd, _ := s.nextDue(last, anchor, earlyWake)
	if !nd.Equal(firstSlot) {
		t.Fatalf("nextDue after early wake = %s, want %s", nd.Format("15:04:05"), firstSlot.Format("15:04:05"))
	}
	if d := nd.Sub(earlyWake); d != 10*time.Second {
		t.Fatalf("time-to-next = %s, want 10s (grid must not drift by elapsed-before-suspend)", d)
	}
}
