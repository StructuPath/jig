// cron_test.go — the five-field parser and, above all, the DST semantics
// (U6, R13). The DST cases are the reason this cron is hand-rolled, so they
// are asserted against real IANA transitions rather than a synthetic zone:
// America/Denver falls back on 1 November 2026 and springs forward on
// 8 March 2026.
package controlplane

import (
	"testing"
	"time"
)

func mustParseCron(t *testing.T, expression, timezone string) cronSchedule {
	t.Helper()
	schedule, _, _, err := parseCronSchedule(expression, timezone)
	if err != nil {
		t.Fatalf("parse %q in %q: %v", expression, timezone, err)
	}
	return schedule
}

// firingsBetween lists every instant the schedule matches in [from, through).
func firingsBetween(t *testing.T, schedule cronSchedule, from, through time.Time) []time.Time {
	t.Helper()
	var firings []time.Time
	cursor := from.Add(-time.Minute)
	for {
		next, err := schedule.Next(cursor)
		if err != nil {
			t.Fatalf("next after %v: %v", cursor, err)
		}
		if !next.Before(through) {
			return firings
		}
		firings = append(firings, next)
		cursor = next
	}
}

// The fall-back overlap hour happens twice on the wall clock, so a daily
// schedule inside it fires twice — once before the transition and once after.
// Skipping the second is a silently dropped hour of coverage for anything
// that runs hourly or on that hour.
func TestADailyScheduleInsideTheFallBackOverlapHourFiresTwice(t *testing.T) {
	schedule := mustParseCron(t, "30 1 * * *", "America/Denver")
	// 1 November 2026: 02:00 MDT (UTC-6) becomes 01:00 MST (UTC-7), so local
	// 01:30 occurs at 07:30 UTC and again at 08:30 UTC.
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	through := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	firings := firingsBetween(t, schedule, from, through)
	want := []time.Time{
		time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC),
	}
	if len(firings) != len(want) {
		t.Fatalf("the overlap hour produced %d firings (%v), want 2", len(firings), firings)
	}
	for index, instant := range firings {
		if !instant.Equal(want[index]) {
			t.Fatalf("firing %d = %v, want %v", index+1, instant, want[index])
		}
		// Both firings render as the same local wall-clock minute; only their
		// UTC offsets differ. That is what "fires twice" means.
		if local := instant.In(schedule.location); local.Hour() != 1 || local.Minute() != 30 {
			t.Fatalf("firing %d renders locally as %v, want 01:30", index+1, local)
		}
	}
}

// The spring-forward gap contains no real instant, so a schedule pinned
// inside it simply does not fire that day. Inventing a substitute instant
// would be a run the operator never scheduled.
func TestAScheduleOnANonexistentLocalMinuteDoesNotFireThatDay(t *testing.T) {
	schedule := mustParseCron(t, "30 2 * * *", "America/Denver")
	// 8 March 2026: 02:00 MST jumps to 03:00 MDT, so local 02:30 never exists.
	springForward := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	dayAfter := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	if firings := firingsBetween(t, schedule, springForward, dayAfter); len(firings) != 0 {
		t.Fatalf("the nonexistent local minute fired at %v, want no firing", firings)
	}
	// The day before and the day after are unaffected: 02:30 MST is 09:30 UTC,
	// 02:30 MDT is 08:30 UTC.
	next, err := schedule.Next(time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if want := time.Date(2026, 3, 7, 9, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("firing before the gap = %v, want %v", next, want)
	}
	next, err = schedule.Next(time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if want := time.Date(2026, 3, 9, 8, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("first firing after the gap = %v, want %v", next, want)
	}
}

// An hourly schedule crossing either transition keeps its cadence in LOCAL
// terms: 25 local hours on the fall-back day, 23 on the spring-forward day.
func TestAnHourlyScheduleGainsAnHourAtFallBackAndLosesOneAtSpringForward(t *testing.T) {
	schedule := mustParseCron(t, "0 * * * *", "America/Denver")
	fallBack := firingsBetween(t,
		schedule,
		time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC), // 2026-11-01 00:00 MDT
		time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC), // 2026-11-02 00:00 MST
	)
	if len(fallBack) != 25 {
		t.Fatalf("the fall-back day produced %d hourly firings, want 25", len(fallBack))
	}
	springForward := firingsBetween(t,
		schedule,
		time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC), // 2026-03-08 00:00 MST
		time.Date(2026, 3, 9, 6, 0, 0, 0, time.UTC), // 2026-03-09 00:00 MDT
	)
	if len(springForward) != 23 {
		t.Fatalf("the spring-forward day produced %d hourly firings, want 23", len(springForward))
	}
}

func TestCronFieldsParseWildcardsListsRangesStepsAndNames(t *testing.T) {
	for _, testCase := range []struct {
		expression string
		after      time.Time
		want       time.Time
	}{
		{"*/15 * * * *",
			time.Date(2026, 8, 6, 10, 3, 0, 0, time.UTC),
			time.Date(2026, 8, 6, 10, 15, 0, 0, time.UTC)},
		{"0 9,17 * * *",
			time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 6, 17, 0, 0, 0, time.UTC)},
		{"0 0 1 JAN *",
			time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
			time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"30 8 * * MON-FRI",
			time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC), // a Saturday
			time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)},
		// Sunday is both 0 and 7.
		{"0 12 * * 7",
			time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)},
		// Both day fields restricted: cron's OR rule, not AND — the 15th or
		// any Monday, whichever comes first.
		{"0 6 15 * MON",
			time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC)},
	} {
		t.Run(testCase.expression, func(t *testing.T) {
			schedule := mustParseCron(t, testCase.expression, "UTC")
			next, err := schedule.Next(testCase.after)
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			if !next.Equal(testCase.want) {
				t.Fatalf("next after %v = %v, want %v", testCase.after, next, testCase.want)
			}
		})
	}
}

func TestInvalidCronExpressionsAndTimezonesAreRejectedNamingTheFault(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		expression string
		timezone   string
	}{
		{"four fields", "* * * *", "UTC"},
		{"six fields", "* * * * * *", "UTC"},
		{"macro", "@hourly", "UTC"},
		{"quartz last-day", "0 0 L * *", "UTC"},
		{"quartz nth-weekday", "0 0 * * 1#2", "UTC"},
		{"quartz no-specific", "0 0 ? * *", "UTC"},
		{"inline timezone", "CRON_TZ=UTC 0 0 * * *", "UTC"},
		{"minute out of range", "60 * * * *", "UTC"},
		{"inverted range", "0 17-9 * * *", "UTC"},
		{"zero step", "*/0 * * * *", "UTC"},
		{"empty list item", "0,,5 * * * *", "UTC"},
		{"empty timezone", "* * * * *", ""},
		{"machine-local timezone", "* * * * *", "Local"},
		{"unknown timezone", "* * * * *", "Mars/Olympus"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, _, _, err := parseCronSchedule(testCase.expression, testCase.timezone); err == nil {
				t.Fatalf("parse %q in %q was accepted", testCase.expression, testCase.timezone)
			}
		})
	}
}

// A syntactically valid expression that can never match must fail at parse
// time rather than becoming a scheduler that walks ten years of minutes on
// every tick.
func TestAnExpressionThatCanNeverMatchFailsRatherThanSpinning(t *testing.T) {
	schedule := mustParseCron(t, "0 0 30 2 *", "UTC")
	if _, err := schedule.Next(time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("February 30th resolved to an instant")
	}
}

// Parsing canonicalizes both halves, because the trigger stores what it will
// execute rather than what was typed.
func TestParsingCanonicalizesTheExpressionAndTimezone(t *testing.T) {
	_, expression, timezone, err := parseCronSchedule("  0   9  *  *  mon ", "America/Denver")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if expression != "0 9 * * mon" {
		t.Fatalf("canonical expression = %q", expression)
	}
	if timezone != "America/Denver" {
		t.Fatalf("canonical timezone = %q", timezone)
	}
}
