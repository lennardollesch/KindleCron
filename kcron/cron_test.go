package main

import (
	"testing"
	"time"
)

func mustCron(t *testing.T, spec string) cronExpr {
	t.Helper()
	expr, err := parseCron(spec)
	if err != nil {
		t.Fatalf("parseCron(%q): %v", spec, err)
	}
	return expr
}

func TestParseCronFieldCount(t *testing.T) {
	// 5 fields: seconds default to 0.
	five := mustCron(t, "0 9 * * 1")
	if five.sec != 1<<0 {
		t.Fatalf("5-field seconds mask = %b, want bit 0 only", five.sec)
	}
	// 6 fields: leading field is seconds.
	six := mustCron(t, "30 0 9 * * 1")
	if six.sec != 1<<30 {
		t.Fatalf("6-field seconds mask = %b, want bit 30 only", six.sec)
	}
	for _, spec := range []string{"", "* * * *", "* * * * * * *"} {
		if _, err := parseCron(spec); err == nil {
			t.Fatalf("parseCron(%q) = nil error, want field-count error", spec)
		}
	}
}

func TestParseCronFieldSyntax(t *testing.T) {
	// Names, ranges, lists, steps, and the dow 7 == 0 fold.
	expr := mustCron(t, "0 30 7 * jan-mar mon,wed,fri")
	if expr.month != 1<<1|1<<2|1<<3 {
		t.Fatalf("month mask = %b, want jan-mar", expr.month)
	}
	if expr.dow != 1<<1|1<<3|1<<5 {
		t.Fatalf("dow mask = %b, want mon/wed/fri", expr.dow)
	}
	if sunday := mustCron(t, "0 0 * * 7"); sunday.dow != 1<<0 {
		t.Fatalf("dow 7 mask = %b, want bit 0 (Sunday)", sunday.dow)
	}
	// 5-field: the leading field is the minute, so */15 sets minutes 0,15,30,45.
	everyQuarter := mustCron(t, "*/15 * * * *")
	if everyQuarter.min != 1<<0|1<<15|1<<30|1<<45 {
		t.Fatalf("*/15 minute mask = %b, want 0,15,30,45", everyQuarter.min)
	}

	for _, spec := range []string{
		"60 * * * *",  // second/minute out of range
		"* 24 * * *",  // hour out of range
		"* * 0 * *",   // dom below 1
		"* * * 13 *",  // month out of range
		"* * * * 8",   // dow above 7
		"*/0 * * * *", // zero step
		"5-1 * * * *", // inverted range
		"* * * xyz *", // bad name
	} {
		if _, err := parseCron(spec); err == nil {
			t.Fatalf("parseCron(%q) = nil error, want range/syntax error", spec)
		}
	}
}

func TestCronNext(t *testing.T) {
	utc := time.UTC
	d := func(y int, mo time.Month, day, h, mi, s int) time.Time {
		return time.Date(y, mo, day, h, mi, s, 0, utc)
	}
	cases := []struct {
		spec string
		from time.Time
		want time.Time
	}{
		// 2026-06-22 is a Monday; 06-24 Wed, 06-27 Sat, 06-29 Mon.
		{"0 9 * * 1", d(2026, 6, 24, 12, 0, 0), d(2026, 6, 29, 9, 0, 0)}, // next Monday 09:00
		{"0 9 * * 1", d(2026, 6, 22, 8, 0, 0), d(2026, 6, 22, 9, 0, 0)},  // same Monday, later today
		{"*/15 * * * *", d(2026, 6, 22, 10, 7, 0), d(2026, 6, 22, 10, 15, 0)},
		{"0/30 * * * * *", d(2026, 6, 22, 10, 0, 10), d(2026, 6, 22, 10, 0, 30)}, // every 30s
		{"0/30 * * * * *", d(2026, 6, 22, 10, 0, 40), d(2026, 6, 22, 10, 1, 0)},
		{"0 0 1 * *", d(2026, 6, 22, 0, 0, 0), d(2026, 7, 1, 0, 0, 0)},       // 1st of next month
		{"30 7 * * 1-5", d(2026, 6, 27, 12, 0, 0), d(2026, 6, 29, 7, 30, 0)}, // Sat -> Mon 07:30
	}
	for _, c := range cases {
		got := mustCron(t, c.spec).next(c.from)
		if !got.Equal(c.want) {
			t.Errorf("(%q).next(%s) = %s, want %s", c.spec, c.from, got, c.want)
		}
	}
}

func TestCronDomDowOr(t *testing.T) {
	// "0 0 13 * 5": the 13th OR any Friday, at midnight.
	expr := mustCron(t, "0 0 13 * 5")
	// 2026-06-13 is a Saturday (not Friday) -> matches via day-of-month.
	if got := expr.next(time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("DOM side: got %s, want 2026-06-13 00:00", got)
	}
	// 2026-06-05 is a Friday (not the 13th) -> matches via day-of-week.
	if got := expr.next(time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("DOW side: got %s, want 2026-06-05 00:00", got)
	}
}

func TestCronImpossible(t *testing.T) {
	// February 30th never occurs.
	if got := mustCron(t, "0 0 30 2 *").next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); !got.IsZero() {
		t.Fatalf("impossible cron next = %s, want zero time", got)
	}
}

func TestCronPrev(t *testing.T) {
	expr := mustCron(t, "0 9 * * *") // daily 09:00
	if got := expr.prev(time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("prev after 09:00 = %s, want same day 09:00", got)
	}
	if got := expr.prev(time.Date(2026, 6, 22, 8, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 6, 21, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("prev before 09:00 = %s, want previous day 09:00", got)
	}
}

func TestCronDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no tzdata for Europe/Berlin")
	}
	// 2026-03-29 02:00-02:59 does not exist in Berlin (spring forward). A daily
	// 02:30 job has no valid slot that day; next lands on the following day.
	c := mustCron(t, "30 2 * * *")
	from := time.Date(2026, 3, 28, 12, 0, 0, 0, berlin)
	got := c.next(from)
	if got.Hour() == 2 && got.Day() == 29 {
		t.Fatalf("next = %s, expected to skip the nonexistent 02:30 on 2026-03-29", got)
	}
	if got.Before(from) {
		t.Fatalf("next = %s, want a time after %s", got, from)
	}
}

func TestScheduleCronDueAndStamp(t *testing.T) {
	utc := time.UTC
	sched, err := parseSchedule("cron 0 9 * * *") // daily 09:00
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2026, 6, 20, 0, 0, 0, 0, utc)
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, utc)

	// 09:00 today passed and was never handled (last unset) -> due.
	if !sched.isDue(time.Unix(0, 0), anchor, now) {
		t.Fatal("cron job should be due after an unhandled slot")
	}
	// fireStamp snaps to the most recent slot (today 09:00).
	want := time.Date(2026, 6, 22, 9, 0, 0, 0, utc)
	if stamp := sched.fireStamp(time.Unix(0, 0), anchor, now); !stamp.Equal(want) {
		t.Fatalf("fireStamp = %s, want %s", stamp, want)
	}
	// After firing today's slot, next due is tomorrow 09:00.
	due, ok := sched.nextDue(want, anchor, now)
	wantNext := time.Date(2026, 6, 23, 9, 0, 0, 0, utc)
	if !ok || !due.Equal(wantNext) {
		t.Fatalf("nextDue = %s ok=%v, want %s", due, ok, wantNext)
	}
	// Not due again until that slot arrives.
	if sched.isDue(want, anchor, now) {
		t.Fatal("cron job should not be due right after its slot was stamped")
	}
}

func TestAtEquivalentToCron(t *testing.T) {
	utc := time.UTC
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, utc)
	at, err := parseSchedule("at 07:00")
	if err != nil {
		t.Fatal(err)
	}
	cron, err := parseSchedule("cron 0 7 * * *")
	if err != nil {
		t.Fatal(err)
	}
	atDue, _ := at.nextDue(now, now, now)
	cronDue, _ := cron.nextDue(now, now, now)
	if !atDue.Equal(cronDue) {
		t.Fatalf("at nextDue %s != cron nextDue %s", atDue, cronDue)
	}
}

func TestShortCronGap(t *testing.T) {
	from := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	threshold := 3 * time.Minute
	// Every 30 seconds: the next fire is imminent and so is the one after, so the
	// device is held awake back-to-back. Gap is 30s, below the threshold.
	if gap, ok := shortCronGap(mustCron(t, "*/30 * * * * *"), threshold, from); !ok || gap != 30*time.Second {
		t.Fatalf("30s cron gap = %s ok=%v, want 30s true", gap, ok)
	}
	// Hourly and weekly cadences are not short-cadence under a 3m threshold.
	if _, ok := shortCronGap(mustCron(t, "0 * * * *"), threshold, from); ok {
		t.Fatal("hourly cron should not be flagged short-cadence")
	}
	if _, ok := shortCronGap(mustCron(t, "0 9 * * 1"), threshold, from); ok {
		t.Fatal("weekly cron should not be flagged short-cadence")
	}

	// Regression: dense bursts whose NEXT fire is far off must NOT be flagged.
	// "* * * 1 * *" fires every second, but only on the 1st; from June 22 the next
	// fire is ~9 days out, so the device sleeps until then.
	if gap, ok := shortCronGap(mustCron(t, "* * * 1 * *"), threshold, from); ok {
		t.Fatalf("monthly-burst cron flagged short-cadence (gap %s); next fire is days away", gap)
	}
	// "*/10 0 14 22 6 *" fires every 10s for one minute, once a year. After this
	// year's burst (June 22, here 15:00) the next fire is ~1 year out.
	annual := mustCron(t, "*/10 0 14 22 6 *")
	if gap, ok := shortCronGap(annual, threshold, time.Date(2026, 6, 22, 15, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("annual-burst cron flagged short-cadence (gap %s); next fire is a year away", gap)
	}
	// But just before the burst it IS flagged: the next fire (and the one after)
	// fall within the threshold, so the device really does stay awake for it.
	if gap, ok := shortCronGap(annual, threshold, time.Date(2026, 6, 22, 13, 59, 0, 0, time.UTC)); !ok || gap != 10*time.Second {
		t.Fatalf("imminent annual burst gap = %s ok=%v, want 10s true", gap, ok)
	}
}

func TestParseScheduleBareCron(t *testing.T) {
	// A bare expression with no keyword is treated as cron, equivalent to the
	// explicit "cron " prefix.
	bare, err := parseSchedule("0 9 * * 1")
	if err != nil {
		t.Fatalf("bare cron parse: %v", err)
	}
	if bare.kind != kCron {
		t.Fatalf("bare expression kind = %d, want kCron", bare.kind)
	}
	keyword, err := parseSchedule("cron 0 9 * * 1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	bareDue, _ := bare.nextDue(now, now, now)
	keywordDue, _ := keyword.nextDue(now, now, now)
	if !bareDue.Equal(keywordDue) {
		t.Fatalf("bare nextDue %s != keyword nextDue %s", bareDue, keywordDue)
	}
	// 6-field bare expression is accepted too.
	if six, err := parseSchedule("*/30 * * * * *"); err != nil || six.kind != kCron {
		t.Fatalf("bare 6-field cron: kind=%d err=%v", six.kind, err)
	}
	// Neither a keyword form nor a valid cron expression -> error.
	if _, err := parseSchedule("not a schedule at all"); err == nil {
		t.Fatal("expected error for non-cron bare input")
	}
}
