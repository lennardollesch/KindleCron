package client

import (
	"testing"
	"time"
)

func TestOnce(t *testing.T) {
	wakeTime := time.Date(2026, 7, 10, 9, 5, 30, 0, time.UTC)
	if got := Once(wakeTime).String(); got != "once 2026-07-10 09:05:30" {
		t.Errorf("Once = %q, want %q", got, "once 2026-07-10 09:05:30")
	}
}

func TestEvery_ChoosesCoarsestExactUnit(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     string
	}{
		{30 * 24 * time.Hour, "every 30d"},
		{24 * time.Hour, "every 1d"},
		{6 * time.Hour, "every 6h"},
		{90 * time.Minute, "every 90m"},
		{30 * time.Minute, "every 30m"},
		{45 * time.Second, "every 45s"},
		{25 * time.Hour, "every 25h"},          // not a whole day -> hours
		{time.Hour + time.Minute, "every 61m"}, // not whole hours -> minutes
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := Every(tc.interval).String(); got != tc.want {
				t.Errorf("Every(%s) = %q, want %q", tc.interval, got, tc.want)
			}
		})
	}
}

func TestEvery_TruncatesSubSecond(t *testing.T) {
	if got := Every(30*time.Second + 500*time.Millisecond).String(); got != "every 30s" {
		t.Errorf("Every with sub-second part = %q, want %q", got, "every 30s")
	}
}

func TestAt(t *testing.T) {
	morning := time.Date(2026, 7, 10, 7, 30, 0, 0, time.UTC)
	evening := time.Date(2026, 7, 10, 19, 5, 0, 0, time.UTC)

	if got := At(morning).String(); got != "at 07:30" {
		t.Errorf("At(one) = %q, want %q", got, "at 07:30")
	}
	if got := At(morning, evening).String(); got != "at 07:30,19:05" {
		t.Errorf("At(two) = %q, want %q", got, "at 07:30,19:05")
	}
}

func TestCron_PassesExpressionThrough(t *testing.T) {
	if got := Cron("0 9 * * 1").String(); got != "cron 0 9 * * 1" {
		t.Errorf("Cron = %q, want %q", got, "cron 0 9 * * 1")
	}
}
