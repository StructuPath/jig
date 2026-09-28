// report.go — `jig report` (plan 2026-09-28-001, U3): the operator's surface
// over GET /api/report. The control plane computes the report (KTD3); this
// command only chooses the window and renders it.
//
// It follows def.go's conventions: flags before positionals, --json prints
// the API's own object verbatim, and exit codes 0/1/2.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/StructuPath/jig/internal/controlplane"
)

const reportUsage = `Usage: jig report [flags]

Summarize the factory's quality over a time window: jobs by terminal state,
accepts, publish outcomes, CI repair, flaky-check re-runs, and spend. The
control plane computes it from its ledger; nothing is written.

Flags:
  --server <url>   control plane URL (default http://127.0.0.1:8383)
  --since <when>   window start: a duration back from now (7d, 24h, 90m) or
                   an RFC3339 timestamp (default: 7 days before --until)
  --until <when>   window end, RFC3339 (default: now)
  --json           emit the API object instead of the prose report

A job is in the window when it is terminal and was last updated in
[since, until). A rate with nothing to divide by prints n/a.

Exit codes:
  0  success
  1  the control plane could not be reached, or rejected the request
  2  usage error
`

func reportCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stdout, reportUsage)
		return exitAccepted
	}
	flags, server, asJSON := defFlags("jig report", stderr, reportUsage)
	sinceFlag := flags.String("since", "", "window start: a duration back from now or RFC3339")
	untilFlag := flags.String("until", "", "window end, RFC3339")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "jig report: unexpected argument %q\n\n%s", flags.Arg(0), reportUsage)
		return exitUsage
	}
	query := url.Values{}
	if *sinceFlag != "" {
		since, err := parseReportSince(*sinceFlag, time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "jig report: --since: %v\n\n%s", err, reportUsage)
			return exitUsage
		}
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	if *untilFlag != "" {
		until, err := time.Parse(time.RFC3339, strings.TrimSpace(*untilFlag))
		if err != nil {
			fmt.Fprintf(stderr, "jig report: --until: %q is not an RFC3339 timestamp\n\n%s",
				*untilFlag, reportUsage)
			return exitUsage
		}
		query.Set("until", until.UTC().Format(time.RFC3339))
	}
	path := "/api/report"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	var raw json.RawMessage
	if err := apiCall(ctx, *server, http.MethodGet, path, nil, &raw); err != nil {
		fmt.Fprintf(stderr, "jig report: %v\n", err)
		return exitInfraFailed
	}
	if *asJSON {
		fmt.Fprintf(stdout, "%s\n", raw)
		return exitAccepted
	}
	var report controlplane.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		fmt.Fprintf(stderr, "jig report: decode response: %v\n", err)
		return exitInfraFailed
	}
	printReport(stdout, report)
	return exitAccepted
}

// parseReportSince reads --since: a positive duration back from now — Go's
// units plus whole days (7d) — or an RFC3339 timestamp.
func parseReportSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at, nil
	}
	var span time.Duration
	if days, found := strings.CutSuffix(value, "d"); found {
		count, err := strconv.Atoi(days)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q is not a duration (7d, 24h, 90m) or an RFC3339 timestamp", value)
		}
		span = time.Duration(count) * 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q is not a duration (7d, 24h, 90m) or an RFC3339 timestamp", value)
		}
		span = parsed
	}
	if span <= 0 {
		return time.Time{}, fmt.Errorf("%q must be a positive duration", value)
	}
	return now.Add(-span), nil
}

// printReport renders the report as one short table per section, then the
// two lines that say what the numbers cannot (R4) and what spend misses.
func printReport(w io.Writer, report controlplane.Report) {
	fmt.Fprintf(w, "jig report  %s → %s\n",
		report.Since.UTC().Format(time.RFC3339), report.Until.UTC().Format(time.RFC3339))

	section(w, "Jobs", [][2]string{
		{"terminal", count(report.Jobs.Total)},
		{"accepted", count(report.Jobs.Accepted)},
		{"accepted, unpublished", count(report.Jobs.AcceptedUnpublished)},
		{"failed", count(report.Jobs.Failed)},
		{"cancelled", count(report.Jobs.Cancelled)},
	})

	accepts := report.Accepts
	section(w, "Accepts", [][2]string{
		{"clean", count(accepts.Clean)},
		{"  green CI on a head jig pushed", count(accepts.CIGreen)},
		{"  definition does not wait for CI", count(accepts.NoCIWait)},
		{"person-fixed (green CI on a head jig did not push)", count(accepts.PersonFixed)},
		{"unverified", count(accepts.Unverified)},
	})

	publish := report.Publish
	rows := [][2]string{
		{"eligible", count(publish.Eligible)},
		{"published", count(publish.Published)},
		{"held", count(publish.Held)},
		{"held rate", rate(publish.HeldRate)},
		{"not attempted", count(publish.NotAttempted)},
		{"failed", count(publish.Failed)},
	}
	rows = append(rows, codes("  ", publish.FailedCodes)...)
	rows = append(rows, [2]string{"unreadable", count(publish.Unreadable)})
	section(w, "Publish", rows)

	ci := report.CI
	repair := ci.Repair
	rows = [][2]string{
		{"attempts that waited for CI", count(ci.Waited)},
		{"first-pass green", count(ci.FirstPassGreen)},
		{"entered repair", count(repair.Entered)},
		{"entry rate", rate(repair.EntryRate)},
		{"repaired to green", count(repair.Succeeded)},
		{"success rate", rate(repair.SuccessRate)},
		{"rounds pushed", count(repair.Rounds)},
	}
	if len(repair.StopCodes) > 0 {
		rows = append(rows, [2]string{"stopped on", ""})
		rows = append(rows, codes("  ", repair.StopCodes)...)
	}
	rows = append(rows,
		[2]string{"ci_timeout", fmt.Sprintf("%d (%s)", ci.Revisit.CITimeouts, rate(ci.Revisit.CITimeoutRate))},
		[2]string{"retried, still red", fmt.Sprintf("%d (%s)", ci.Revisit.RetryStillRed, rate(ci.Revisit.RetryStillRedRate))},
	)
	section(w, "CI repair", rows)

	section(w, "Re-runs", [][2]string{
		{"attempts with a re-run", count(ci.Reruns.Attempts)},
		{"re-runs", count(ci.Reruns.Reruns)},
		{"flaky passes (green after a re-run)", count(ci.Reruns.FlakyPasses)},
	})

	spend := report.Spend
	rows = [][2]string{
		{"total", usd(spend.TotalUSD)},
		{"per clean accept", usdPointer(spend.PerCleanAcceptUSD)},
	}
	buckets := make([]string, 0, len(spend.ByOutcome))
	for name := range spend.ByOutcome {
		buckets = append(buckets, name)
	}
	sort.Strings(buckets)
	for _, name := range buckets {
		bucket := spend.ByOutcome[name]
		if bucket.Jobs == 0 {
			continue
		}
		rows = append(rows, [2]string{"  " + strings.ReplaceAll(name, "_", " "),
			fmt.Sprintf("%s over %d job(s)", usd(bucket.CostUSD), bucket.Jobs)})
	}
	section(w, "Spend", rows)

	if report.UnreadableResults > 0 {
		fmt.Fprintf(w, "\n%d result(s) could not be read in full and are counted, not guessed.\n",
			report.UnreadableResults)
	} else {
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "Not seen: jig does not watch CI after accept, so there is no post-merge CI rate; "+
		"person-fixed accepts and accepts that did not wait for CI are counted separately.")
	if spend.UnmeteredSends > 0 {
		fmt.Fprintf(w, "Spend undercounts: %d unmetered send(s) were killed before returning a cost, "+
			"and spend recorded by jig v0.2.0 or earlier omits phases that did not pass.\n",
			spend.UnmeteredSends)
	} else {
		fmt.Fprintln(w, "Unmetered sends: 0. Spend recorded by jig v0.2.0 or earlier omits phases that did not pass.")
	}
}

func section(w io.Writer, title string, rows [][2]string) {
	fmt.Fprintf(w, "\n%s\n", title)
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		fmt.Fprintf(table, "  %s\t%s\n", row[0], row[1])
	}
	table.Flush()
}

func codes(indent string, byCode map[string]int) [][2]string {
	names := make([]string, 0, len(byCode))
	for name := range byCode {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([][2]string, 0, len(names))
	for _, name := range names {
		rows = append(rows, [2]string{indent + name, count(byCode[name])})
	}
	return rows
}

func count(value int) string { return strconv.Itoa(value) }

// rate prints a fraction as a percentage; a null rate had nothing to divide
// by, and a zero there would read as a measurement.
func rate(value *float64) string {
	if value == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", *value*100)
}

func usd(value float64) string { return fmt.Sprintf("$%.2f", value) }

func usdPointer(value *float64) string {
	if value == nil {
		return "n/a"
	}
	return usd(*value)
}
