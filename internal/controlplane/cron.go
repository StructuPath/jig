// cron.go — a hand-rolled five-field cron with IANA timezones and explicit
// DST semantics (U6, R13).
//
// Hand-rolled because the DST behavior is a decision, not a detail, and a
// dependency's decision is not visible in this repository. The whole design
// is one choice: THE SEARCH RUNS IN UTC AND THE MATCH RUNS IN LOCAL TIME.
// Next walks forward one UTC minute at a time and asks whether that instant,
// rendered in the trigger's zone, satisfies the five fields. Both DST rules
// then fall out of the walk rather than being special-cased:
//
//   - An overlap hour FIRES TWICE. When a zone falls back, two distinct UTC
//     minutes render as the same local wall-clock minute; the walk visits
//     both, and both match. "02:30 daily" on the US fall-back day runs at
//     02:30 MDT and again at 02:30 MST — an hourly backup that skipped one of
//     them would silently drop an hour of coverage.
//   - A nonexistent local minute NEVER FIRES. When a zone springs forward,
//     no UTC instant renders as 02:30 local, so the walk never finds one and
//     that day has no firing. The alternative — inventing a substitute
//     instant — is a fire the operator did not schedule.
//
// A library built on time.Date(year, month, day, hour, minute, ...) in the
// local zone cannot express either rule: Go's normalization silently maps a
// nonexistent local time onto a real one, and it cannot name the second of
// two identical wall-clock minutes at all.
//
// Stdlib only. time/tzdata is embedded so an IANA name resolves on a machine
// with no system zoneinfo — the schedule must mean the same thing everywhere
// the binary runs.
package controlplane

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

// cronSearchYears bounds the forward walk. A five-field expression that
// matches nothing within a decade (February 30th, say) is a save-time error,
// not a scheduler that spins.
const cronSearchYears = 10

// cronField is one parsed field: the set of values it admits, plus whether it
// was written as a wildcard — day-of-month and day-of-week need that
// distinction to reproduce cron's OR rule.
type cronField struct {
	allowed  []bool
	wildcard bool
}

type cronSchedule struct {
	minute     cronField
	hour       cronField
	dayOfMonth cronField
	month      cronField
	dayOfWeek  cronField
	location   *time.Location
}

var cronMonths = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var cronWeekdays = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// parseCronSchedule parses a five-field expression against an IANA timezone,
// returning the schedule plus the canonical forms of both — canonical because
// the trigger config stores what it will execute, not what was typed.
//
// The unsupported-syntax rejections are deliberate: `@hourly`, `L`, `W`, `#`,
// and `?` are Quartz extensions with no single agreed meaning. Accepting them
// silently under some other interpretation is how a schedule comes to fire on
// a day nobody chose.
func parseCronSchedule(expression, timezone string) (cronSchedule, string, string, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return cronSchedule{}, "", "", fmt.Errorf("cron must contain exactly five fields")
	}
	upperExpression := strings.ToUpper(expression)
	if strings.Contains(upperExpression, "CRON_TZ") {
		return cronSchedule{}, "", "", fmt.Errorf("timezone must be supplied separately, not inline as CRON_TZ")
	}
	// Month and weekday names legitimately contain letters the syntax check
	// rejects (SEP has no 'L' but MAR has none either — WED has 'W'), so they
	// are removed before the check rather than exempted from it.
	syntaxOnly := upperExpression
	for name := range cronMonths {
		syntaxOnly = strings.ReplaceAll(syntaxOnly, name, "")
	}
	for name := range cronWeekdays {
		syntaxOnly = strings.ReplaceAll(syntaxOnly, name, "")
	}
	for _, rejected := range []string{"@", "?", "L", "W", "#"} {
		if strings.Contains(syntaxOnly, rejected) {
			return cronSchedule{}, "", "", fmt.Errorf("cron contains unsupported syntax %q", rejected)
		}
	}
	canonicalTimezone := strings.TrimSpace(timezone)
	if canonicalTimezone == "" || canonicalTimezone == "Local" {
		// "Local" is the operator's machine, which is not a property of the
		// schedule: the same trigger must mean the same instants on the
		// laptop and on whatever runs it next.
		return cronSchedule{}, "", "", fmt.Errorf("timezone must be a valid IANA timezone, not %q", timezone)
	}
	location, err := time.LoadLocation(canonicalTimezone)
	if err != nil {
		return cronSchedule{}, "", "", fmt.Errorf("timezone must be a valid IANA timezone: %w", err)
	}
	minute, err := parseCronField(fields[0], 0, 59, nil, false)
	if err != nil {
		return cronSchedule{}, "", "", fmt.Errorf("minute field: %w", err)
	}
	hour, err := parseCronField(fields[1], 0, 23, nil, false)
	if err != nil {
		return cronSchedule{}, "", "", fmt.Errorf("hour field: %w", err)
	}
	dayOfMonth, err := parseCronField(fields[2], 1, 31, nil, false)
	if err != nil {
		return cronSchedule{}, "", "", fmt.Errorf("day-of-month field: %w", err)
	}
	month, err := parseCronField(fields[3], 1, 12, cronMonths, false)
	if err != nil {
		return cronSchedule{}, "", "", fmt.Errorf("month field: %w", err)
	}
	dayOfWeek, err := parseCronField(fields[4], 0, 7, cronWeekdays, true)
	if err != nil {
		return cronSchedule{}, "", "", fmt.Errorf("day-of-week field: %w", err)
	}
	return cronSchedule{
		minute: minute, hour: hour, dayOfMonth: dayOfMonth,
		month: month, dayOfWeek: dayOfWeek, location: location,
	}, strings.Join(fields, " "), canonicalTimezone, nil
}

// parseCronField parses one field: comma lists of wildcards, values, ranges,
// and steps. sundaySeven folds day-of-week 7 onto 0, the one place cron's
// value domain overlaps itself.
func parseCronField(value string, minimum, maximum int, names map[string]int, sundaySeven bool) (cronField, error) {
	field := cronField{allowed: make([]bool, maximum+1), wildcard: strings.HasPrefix(value, "*")}
	if value == "" {
		return field, fmt.Errorf("field is empty")
	}
	for _, item := range strings.Split(value, ",") {
		if item == "" {
			return field, fmt.Errorf("empty list item")
		}
		base, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			if stepText == "" || strings.Contains(stepText, "/") {
				return field, fmt.Errorf("invalid step in %q", item)
			}
			parsed, err := strconv.Atoi(stepText)
			if err != nil || parsed < 1 {
				return field, fmt.Errorf("step in %q must be a positive integer", item)
			}
			step = parsed
		}
		start, end := minimum, maximum
		switch {
		case base == "*":
		case strings.Contains(base, "-"):
			left, right, ok := strings.Cut(base, "-")
			if !ok || left == "" || right == "" || strings.Contains(right, "-") {
				return field, fmt.Errorf("invalid range in %q", item)
			}
			var err error
			if start, err = parseCronValue(left, minimum, maximum, names); err != nil {
				return field, err
			}
			if end, err = parseCronValue(right, minimum, maximum, names); err != nil {
				return field, err
			}
			if start > end {
				return field, fmt.Errorf("range start must not exceed range end in %q", item)
			}
		default:
			parsed, err := parseCronValue(base, minimum, maximum, names)
			if err != nil {
				return field, err
			}
			start, end = parsed, parsed
			if hasStep {
				// "5/15" means "from 5, every 15" — the step turns a single
				// value into an open-ended series.
				end = maximum
			}
		}
		for candidate := start; candidate <= end; candidate += step {
			if sundaySeven && candidate == 7 {
				field.allowed[0] = true
			} else {
				field.allowed[candidate] = true
			}
		}
	}
	for candidate := minimum; candidate <= maximum; candidate++ {
		if field.allowed[candidate] {
			return field, nil
		}
	}
	return field, fmt.Errorf("field %q selects no values", value)
}

func parseCronValue(value string, minimum, maximum int, names map[string]int) (int, error) {
	if named, ok := names[strings.ToUpper(value)]; ok {
		return named, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("value %q must be between %d and %d", value, minimum, maximum)
	}
	return parsed, nil
}

// Next returns the first matching instant strictly after `after`, as a UTC
// instant truncated to the minute. The walk is what implements both DST rules
// (see the file comment); every returned value is a real instant, and two
// consecutive returns may render as the same local wall clock.
func (schedule cronSchedule) Next(after time.Time) (time.Time, error) {
	candidate := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := candidate.AddDate(cronSearchYears, 0, 0)
	for !candidate.After(limit) {
		if schedule.matches(candidate) {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("cron has no matching instant within %d years", cronSearchYears)
}

// matches renders one UTC instant in the schedule's zone and tests the five
// fields against the local rendering. Day-of-month and day-of-week follow
// cron's OR rule: when both are restricted, either one matching is enough.
func (schedule cronSchedule) matches(instant time.Time) bool {
	local := instant.In(schedule.location)
	if !schedule.minute.allowed[local.Minute()] || !schedule.hour.allowed[local.Hour()] ||
		!schedule.month.allowed[int(local.Month())] {
		return false
	}
	dayOfMonth := schedule.dayOfMonth.allowed[local.Day()]
	dayOfWeek := schedule.dayOfWeek.allowed[int(local.Weekday())]
	switch {
	case schedule.dayOfMonth.wildcard && schedule.dayOfWeek.wildcard:
		return true
	case schedule.dayOfMonth.wildcard:
		return dayOfWeek
	case schedule.dayOfWeek.wildcard:
		return dayOfMonth
	default:
		return dayOfMonth || dayOfWeek
	}
}
