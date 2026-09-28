// report_test.go — the quality report (plan 2026-09-28-001, U2). The ledger
// is seeded row by row, including result fields other units add in parallel
// (`publish_history`, `ci_reruns`, `unmetered_sends`), so every count below
// is exact and known.
package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// reportFixture seeds report rows straight into the ledger.
type reportFixture struct {
	t     *testing.T
	store *Store
	seq   map[string]int64
}

const (
	reportRunCI   = "run-ci"
	reportRunNoCI = "run-noci"
)

func newReportFixture(t *testing.T) (*reportFixture, *testClock) {
	t.Helper()
	store, clock := newTestStore(t)
	seedRun(t, store, reportRunCI)
	useSnapshot(t, store, reportRunCI, repairFixtureSnapshot)
	seedRun(t, store, reportRunNoCI)
	return &reportFixture{t: t, store: store, seq: map[string]int64{}}, clock
}

func (f *reportFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.db.Exec(query, args...); err != nil {
		f.t.Fatalf("seed %q: %v", query, err)
	}
}

// job inserts a job in state, last updated at updated.
func (f *reportFixture) job(id, runID, state string, updated time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO jobs(id, run_id, repository, base_sha, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, runID, "github.com/example/"+id, shaA, state,
		updated.UnixMilli(), updated.UnixMilli())
}

// attempt inserts attempt number n of jobID and returns its id.
func (f *reportFixture) attempt(jobID string, n int, state, result string) string {
	f.t.Helper()
	id := jobID + "-attempt-" + string(rune('0'+n))
	var stored any
	if result != "" {
		stored = result
	}
	f.exec(`INSERT INTO attempts(id, job_id, attempt_number, state, result, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id, jobID, n, state, stored, f.store.now().UnixMilli())
	return id
}

// event stores one event the way ingestion does: the whole marshalled event.
func (f *reportFixture) event(attemptID, eventType string, payload map[string]any) {
	f.t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	stored, err := json.Marshal(protocol.Event{
		Seq: f.seq[attemptID], Type: eventType, Phase: "build", Name: "builder", Payload: body,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO events(attempt_id, seq, type, phase, payload, payload_bytes, server_time)
		VALUES (?, ?, ?, 'build', ?, ?, ?)`, attemptID, f.seq[attemptID], eventType, stored,
		len(stored), f.store.now().UnixMilli())
	f.seq[attemptID]++
}

// spend records one agent_end; unmetered < 0 leaves the field out, as every
// event from before the field existed does.
func (f *reportFixture) spend(attemptID string, cost float64, unmetered int) {
	f.t.Helper()
	payload := map[string]any{"cost": cost, "tokens": 100, "sends": 1}
	if unmetered >= 0 {
		payload["unmetered_sends"] = unmetered
	}
	f.event(attemptID, protocol.EventAgentEnd, payload)
}

func (f *reportFixture) step(attemptID, step, ref string) {
	f.t.Helper()
	f.exec(`INSERT INTO publish_records(attempt_id, step, branch, remote_ref, pr_url, completed_at)
		VALUES (?, ?, 'jig/branch', ?, '', ?)`, attemptID, step, ref, f.store.now().UnixMilli())
}

// rounds records pushed repair rounds chaining through heads.
func (f *reportFixture) rounds(attemptID string, heads ...string) {
	f.t.Helper()
	for index := 1; index < len(heads); index++ {
		f.exec(`INSERT INTO publish_ci_repairs(attempt_id, round, branch, head_before, head_after, failed_checks, completed_at)
			VALUES (?, ?, 'jig/branch', ?, ?, '["test"]', ?)`, attemptID, index, heads[index-1], heads[index],
			f.store.now().UnixMilli())
	}
}

// result builds an attempt result from a publish value and optional history.
func result(t *testing.T, publish any, history ...any) string {
	t.Helper()
	document := map[string]any{"changed_paths": []string{"a.go"}, "publish": publish}
	if history != nil {
		document["publish_history"] = history
	}
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func summary(state, code string, extra map[string]any) map[string]any {
	value := map[string]any{"state": state, "branch": "jig/branch"}
	if code != "" {
		value["code"] = code
	}
	for key, item := range extra {
		value[key] = item
	}
	return value
}

var (
	redChecks     = []map[string]any{{"name": "test", "verdict": "fail"}}
	pendingChecks = []map[string]any{{"name": "test", "verdict": "pending"}}
	twoRounds     = []map[string]any{
		{"round": 1, "head_before": shaA, "head_after": shaB, "outcome": "pushed"},
		{"round": 2, "head_before": shaB, "head_after": shaC, "outcome": "pushed"},
	}
)

func floatPointer(value float64) *float64 { return &value }

func mustReport(t *testing.T, store *Store, since, until time.Time) Report {
	t.Helper()
	report, err := store.Report(context.Background(), since, until)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	return report
}

// seedEveryOutcome seeds one job per outcome the report distinguishes, plus
// jobs just outside the window. The window is [since, until).
func seedEveryOutcome(t *testing.T) (*Store, time.Time, time.Time) {
	f, clock := newReportFixture(t)
	now := clock.Now()
	since, until := now.Add(-time.Hour), now.Add(time.Hour)

	// Clean on the first try, over two attempts: spend counts both.
	f.job("j01-clean", reportRunCI, protocol.JobAccepted, now)
	failedFirst := f.attempt("j01-clean", 1, protocol.AttemptFailed, "")
	f.spend(failedFirst, 0.25, -1)
	f.event(failedFirst, protocol.EventLog, map[string]any{"cost": 50.0})
	clean := f.attempt("j01-clean", 2, protocol.AttemptAccepted, result(t, summary("published", "", nil)))
	f.spend(clean, 0.25, 0)
	f.spend(clean, 0.25, -1)
	f.step(clean, protocol.PublishStepProof, shaA)
	f.step(clean, protocol.PublishStepCI, shaA)

	// Two pushed rounds that ended green.
	f.job("j02-repaired", reportRunCI, protocol.JobAccepted, now)
	repaired := f.attempt("j02-repaired", 1, protocol.AttemptAccepted,
		result(t, summary("published", "", map[string]any{"ci_repairs": twoRounds})))
	f.spend(repaired, 0.5, -1)
	f.step(repaired, protocol.PublishStepProof, shaA)
	f.rounds(repaired, shaA, shaB, shaC)
	f.step(repaired, protocol.PublishStepCI, shaC)

	// Repair exhausted, then a person pushed and the publish-only retry went
	// green on their head: repaired, unsuccessful, and person-fixed.
	f.job("j03-exhausted", reportRunCI, protocol.JobAccepted, now)
	exhausted := f.attempt("j03-exhausted", 1, protocol.AttemptAccepted, result(t,
		summary("published", "", nil),
		summary("failed", "ci_repair_exhausted", map[string]any{"ci_repairs": twoRounds, "ci_failures": redChecks})))
	f.spend(exhausted, 0.25, -1)
	f.step(exhausted, protocol.PublishStepProof, shaA)
	f.rounds(exhausted, shaA, shaB, shaC)
	f.step(exhausted, protocol.PublishStepCI, shaD)

	// Red, retried, still red: the retry-repair signal.
	f.job("j04-still-red", reportRunCI, protocol.JobAcceptedUnpublished, now)
	stillRed := f.attempt("j04-still-red", 1, protocol.AttemptAcceptedUnpublished, result(t,
		summary("failed", "ci_failed", map[string]any{"ci_failures": redChecks}),
		summary("failed", "ci_failed", map[string]any{"ci_failures": redChecks})))
	f.spend(stillRed, 0.25, -1)
	f.step(stillRed, protocol.PublishStepProof, shaA)

	// CI timed out.
	f.job("j05-timeout", reportRunCI, protocol.JobAcceptedUnpublished, now)
	timeout := f.attempt("j05-timeout", 1, protocol.AttemptAcceptedUnpublished,
		result(t, summary("failed", "ci_timeout", map[string]any{"ci_failures": pendingChecks})))
	f.step(timeout, protocol.PublishStepProof, shaA)

	// A pass after a re-run: flaky. Re-run 1 appears in history and current,
	// and counts once.
	f.job("j06-flaky", reportRunCI, protocol.JobAccepted, now)
	flaky := f.attempt("j06-flaky", 1, protocol.AttemptAccepted, result(t,
		summary("published", "", map[string]any{"ci_reruns": []map[string]any{
			{"attempt": 1, "jobs": []int64{11}, "outcome": "failed"},
			{"attempt": 2, "jobs": []int64{11}, "outcome": "passed"},
		}}),
		summary("failed", "publish_unavailable", map[string]any{"ci_reruns": []map[string]any{
			{"attempt": 1, "jobs": []int64{11}, "outcome": "failed"},
		}})))
	f.spend(flaky, 0.5, -1)
	f.step(flaky, protocol.PublishStepProof, shaA)
	f.step(flaky, protocol.PublishStepCI, shaA)

	// Held by the engine marker, before any publish step.
	f.job("j07-held-marker", reportRunCI, protocol.JobAcceptedUnpublished, now)
	f.spend(f.attempt("j07-held-marker", 1, protocol.AttemptAcceptedUnpublished, result(t, "held")), 0.25, -1)

	// Held by the publishing runner's summary.
	f.job("j08-held-summary", reportRunNoCI, protocol.JobAcceptedUnpublished, now)
	f.attempt("j08-held-summary", 1, protocol.AttemptAcceptedUnpublished,
		result(t, summary("held", "publish_held", nil)))

	// Publish never ran.
	f.job("j09-not-attempted", reportRunNoCI, protocol.JobAcceptedUnpublished, now)
	f.attempt("j09-not-attempted", 1, protocol.AttemptAcceptedUnpublished, result(t, "not_attempted"))

	// A result that is not JSON at all.
	f.job("j10-malformed", reportRunNoCI, protocol.JobAcceptedUnpublished, now)
	f.spend(f.attempt("j10-malformed", 1, protocol.AttemptAcceptedUnpublished, `{"publish": {"state"`), 0.125, -1)

	// Accepted under a definition that does not wait for CI.
	f.job("j11-no-ci", reportRunNoCI, protocol.JobAccepted, now)
	noCI := f.attempt("j11-no-ci", 1, protocol.AttemptAccepted, result(t, summary("published", "", nil)))
	f.spend(noCI, 0.25, -1)
	f.step(noCI, protocol.PublishStepProof, shaA)

	// Failed, updated exactly at since: the lower bound is inclusive.
	f.job("j12-failed", reportRunCI, protocol.JobFailed, since)
	f.spend(f.attempt("j12-failed", 1, protocol.AttemptFailed, ""), 1.0, 2)

	f.job("j13-cancelled", reportRunCI, protocol.JobCancelled, now)
	f.spend(f.attempt("j13-cancelled", 1, protocol.AttemptCancelled, ""), 0.125, 1)

	// Clean on the first try, with a history entry that is not a summary.
	f.job("j14-bad-history", reportRunCI, protocol.JobAccepted, now)
	badHistory := f.attempt("j14-bad-history", 1, protocol.AttemptAccepted,
		result(t, summary("published", "", nil), "garbage"))
	f.step(badHistory, protocol.PublishStepProof, shaA)
	f.step(badHistory, protocol.PublishStepCI, shaA)

	// Outside the window: before since, exactly at until, and not terminal.
	for _, outside := range []struct {
		id, state string
		updated   time.Time
	}{
		{"x-before", protocol.JobAccepted, since.Add(-time.Millisecond)},
		{"x-at-until", protocol.JobFailed, until},
		{"x-active", protocol.JobActive, now},
	} {
		f.job(outside.id, reportRunCI, outside.state, outside.updated)
		attempt := f.attempt(outside.id, 1, protocol.AttemptRunning, result(t, "held"))
		f.spend(attempt, 100, 7)
		f.step(attempt, protocol.PublishStepProof, shaA)
		f.step(attempt, protocol.PublishStepCI, shaE)
	}
	return f.store, since, until
}

// Seeded jobs across every terminal state and publish outcome produce exact
// counts; jobs outside [since, until) are excluded.
func TestReportCountsEveryOutcomeExactlyWithinTheWindow(t *testing.T) {
	store, since, until := seedEveryOutcome(t)
	got := mustReport(t, store, since, until)

	want := Report{
		Since: since, Until: until, ObservedAt: store.now().UTC(),
		Jobs:    ReportJobs{Total: 14, Accepted: 6, AcceptedUnpublished: 6, Failed: 1, Cancelled: 1},
		Accepts: ReportAccepts{Clean: 5, CIGreen: 4, NoCIWait: 1, PersonFixed: 1},
		Publish: ReportPublish{
			Eligible: 12, Published: 6, Held: 2, NotAttempted: 1, Failed: 2, Unreadable: 1,
			FailedCodes: map[string]int{"ci_failed": 1, "ci_timeout": 1},
			HeldRate:    floatPointer(2.0 / 12),
		},
		CI: ReportCI{
			Waited:         7,
			FirstPassGreen: 2,
			Repair: ReportCIRepair{
				Entered: 2, EntryRate: floatPointer(2.0 / 7),
				Succeeded: 1, SuccessRate: floatPointer(0.5),
				Rounds:    4,
				StopCodes: map[string]int{"ci_repair_exhausted": 1},
			},
			Reruns: ReportCIReruns{Attempts: 1, Reruns: 2, FlakyPasses: 1},
			Revisit: ReportCIRevisit{
				CITimeouts: 1, CITimeoutRate: floatPointer(1.0 / 7),
				RetryStillRed: 1, RetryStillRedRate: floatPointer(1.0 / 7),
			},
		},
		Spend: ReportSpend{
			TotalUSD: 4.0, UnmeteredSends: 3, PerCleanAcceptUSD: floatPointer(0.8),
			ByOutcome: map[string]ReportSpendBucket{
				spendClean:       {Jobs: 5, CostUSD: 2.0},
				spendPersonFixed: {Jobs: 1, CostUSD: 0.25},
				spendUnverified:  {},
				spendHeld:        {Jobs: 2, CostUSD: 0.25},
				spendUnpublished: {Jobs: 4, CostUSD: 0.375},
				spendFailed:      {Jobs: 1, CostUSD: 1.0, UnmeteredSends: 2},
				spendCancelled:   {Jobs: 1, CostUSD: 0.125, UnmeteredSends: 1},
			},
		},
		UnreadableResults: 2,
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("report mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// An accept the ledger cannot vouch for is never clean: a CI-waiting accept
// with no ci record, and one whose frozen definition no longer parses.
func TestReportNeverCountsAnUnprovableAcceptAsClean(t *testing.T) {
	f, clock := newReportFixture(t)
	now := clock.Now()
	f.job("no-ci-record", reportRunCI, protocol.JobAccepted, now)
	missing := f.attempt("no-ci-record", 1, protocol.AttemptAccepted, result(t, summary("published", "", nil)))
	f.spend(missing, 0.5, -1)
	f.step(missing, protocol.PublishStepProof, shaA)

	seedRun(t, f.store, "run-broken")
	useSnapshot(t, f.store, "run-broken", "not: [a definition")
	f.job("broken-snapshot", "run-broken", protocol.JobAccepted, now)
	broken := f.attempt("broken-snapshot", 1, protocol.AttemptAccepted, result(t, summary("published", "", nil)))
	f.step(broken, protocol.PublishStepProof, shaA)

	got := mustReport(t, f.store, now.Add(-time.Hour), now.Add(time.Hour))
	if got.Accepts != (ReportAccepts{Unverified: 2}) {
		t.Fatalf("accepts = %+v, want 2 unverified and nothing clean", got.Accepts)
	}
	if got.Spend.PerCleanAcceptUSD != nil {
		t.Fatalf("per clean accept = %v, want null with no clean accept", *got.Spend.PerCleanAcceptUSD)
	}
	if bucket := got.Spend.ByOutcome[spendUnverified]; bucket.Jobs != 2 || bucket.CostUSD != 0.5 {
		t.Fatalf("unverified spend = %+v, want 2 jobs, 0.5", bucket)
	}
}

// The empty window is zeros and nulls, never an error, and every spend
// bucket is present.
func TestReportOverAnEmptyWindowIsZeros(t *testing.T) {
	store, clock := newTestStore(t)
	now := clock.Now()
	got := mustReport(t, store, now.Add(-time.Hour), now)
	if got.Jobs != (ReportJobs{}) || got.Accepts != (ReportAccepts{}) || got.UnreadableResults != 0 ||
		got.Spend.TotalUSD != 0 || got.CI.Waited != 0 {
		t.Fatalf("empty report = %+v, want zeros", got)
	}
	if got.Publish.HeldRate != nil || got.CI.Repair.EntryRate != nil || got.CI.Repair.SuccessRate != nil ||
		got.CI.Revisit.CITimeoutRate != nil || got.CI.Revisit.RetryStillRedRate != nil ||
		got.Spend.PerCleanAcceptUSD != nil {
		t.Fatalf("empty report rates = %+v, want every rate null", got)
	}
	if len(got.Spend.ByOutcome) != len(spendBuckets) {
		t.Fatalf("spend buckets = %v, want all %d present", got.Spend.ByOutcome, len(spendBuckets))
	}
}

func getReport(t *testing.T, handler http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("GET", "http://127.0.0.1:8383/api/report"+query, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// GET /api/report returns the store's object for the window it names, and
// defaults to the seven days before now.
func TestReportRouteReturnsTheStoreReport(t *testing.T) {
	store, since, until := seedEveryOutcome(t)
	handler := NewHandler(store, "", discardLogger())

	recorder := getReport(t, handler, "?since="+since.Format(time.RFC3339)+"&until="+until.Format(time.RFC3339))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/report = %d %s", recorder.Code, recorder.Body.String())
	}
	var got Report
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	want := mustReport(t, store, since, until)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("route report = %+v\nwant %+v", got, want)
	}

	// A minute later, so the fixture's jobs (updated at the old now) are
	// inside a window whose end is exclusive.
	store.now = func() time.Time { return since.Add(time.Hour + time.Minute) }
	recorder = getReport(t, handler, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/report (default window) = %d %s", recorder.Code, recorder.Body.String())
	}
	var defaulted Report
	if err := json.Unmarshal(recorder.Body.Bytes(), &defaulted); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	now := store.now().UTC()
	if !defaulted.Until.Equal(now) || !defaulted.Since.Equal(now.Add(-7*24*time.Hour)) {
		t.Fatalf("default window = [%s, %s), want the seven days before %s", defaulted.Since, defaulted.Until, now)
	}
	// The seven-day window takes in x-before (an hour and a millisecond ago)
	// and still leaves out x-at-until (an hour from now) and x-active.
	if defaulted.Jobs.Total != 15 {
		t.Fatalf("default window jobs = %d, want 15", defaulted.Jobs.Total)
	}
}

// A malformed or inverted window is a 400 naming the fault.
func TestReportRouteRejectsABadWindow(t *testing.T) {
	store, clock := newTestStore(t)
	handler := NewHandler(store, "", discardLogger())
	now := clock.Now()
	for _, test := range []struct{ query, code string }{
		{"?since=yesterday", "invalid_query_parameter"},
		{"?until=1754380800000", "invalid_query_parameter"},
		{"?since=" + now.Format(time.RFC3339) + "&until=" + now.Format(time.RFC3339), "invalid_report_window"},
		{"?since=" + now.Format(time.RFC3339) + "&until=" + now.Add(-time.Hour).Format(time.RFC3339), "invalid_report_window"},
	} {
		recorder := getReport(t, handler, test.query)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("GET /api/report%s = %d, want 400", test.query, recorder.Code)
		}
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Error.Code != test.code {
			t.Fatalf("GET /api/report%s error = %s, want %s", test.query, recorder.Body.String(), test.code)
		}
	}
}
