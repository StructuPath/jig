// report.go — the factory quality report (plan 2026-09-28-001, U2, KTD3,
// KTD4). One read-only aggregate over a time window, computed from the
// ledger the control plane already keeps: job states, publish records, CI
// repair rounds, attempt results, and agent_end spend events. Nothing here
// writes, and nothing needs a migration.
//
// What the report can and cannot see (R4):
//   - A clean accept is judged from the publish ledger, never from the
//     worker's result: green CI recorded on a head jig pushed (the proof
//     ref or a repair round's head_after), or proof under a definition that
//     does not wait for CI. Green CI on any other head is a person's fix.
//   - Accepts under a definition that does not wait for CI are clean by
//     definition and counted separately, because jig never saw CI for them.
//   - Nothing here observes the base branch after merge, so the report makes
//     no claim about post-merge CI.
//
// Publish summaries are read from the latest attempt's result: `publish`
// plus the `publish_history` a publish-only retry keeps (R15), oldest first.
// Anything it needs from a result and cannot read is counted, never guessed
// (`unreadable_results`).
package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// defaultReportWindow is the window a report covers when the caller names no
// `since`.
const defaultReportWindow = 7 * 24 * time.Hour

// Report is the quality report over one window. Counts are always numbers;
// a rate or a per-accept figure is null when its denominator is zero, because
// a zero there would read as a measurement.
type Report struct {
	Since      time.Time `json:"since"`
	Until      time.Time `json:"until"`
	ObservedAt time.Time `json:"observed_at"`

	Jobs    ReportJobs    `json:"jobs"`
	Accepts ReportAccepts `json:"accepts"`
	Publish ReportPublish `json:"publish"`
	CI      ReportCI      `json:"ci"`
	Spend   ReportSpend   `json:"spend"`

	// UnreadableResults counts publish-eligible jobs whose latest attempt
	// result could not be read in full: not JSON, a missing or unknown
	// `publish`, or a malformed history, repair, or re-run entry.
	UnreadableResults int `json:"unreadable_results"`
}

// ReportJobs counts the jobs in the window by terminal state. A job is in
// the window when it is terminal and its updated_at falls in [since, until).
type ReportJobs struct {
	Total               int `json:"total"`
	Accepted            int `json:"accepted"`
	AcceptedUnpublished int `json:"accepted_unpublished"`
	Failed              int `json:"failed"`
	Cancelled           int `json:"cancelled"`
}

// ReportAccepts splits accepted jobs by what the ledger proves (KTD4, R4).
// Clean = CIGreen + NoCIWait. PersonFixed and Unverified are never clean.
type ReportAccepts struct {
	Clean int `json:"clean"`
	// CIGreen: green CI recorded on a head jig pushed.
	CIGreen int `json:"clean_ci_green"`
	// NoCIWait: proof under a definition that does not wait for CI; jig
	// never saw CI for these.
	NoCIWait int `json:"clean_no_ci_wait"`
	// PersonFixed: green CI recorded on a head jig did not push.
	PersonFixed int `json:"person_fixed"`
	// Unverified: accepted, but the ledger cannot say which (the frozen
	// definition no longer parses, or a CI-waiting accept has no ci record).
	Unverified int `json:"unverified"`
}

// ReportPublish classifies the latest publish outcome of every accepted or
// accepted_unpublished job. Accepted jobs are published by the ledger's own
// rule (R12); accepted_unpublished jobs are classified from their result.
type ReportPublish struct {
	Eligible     int `json:"eligible"`
	Published    int `json:"published"`
	Held         int `json:"held"`
	NotAttempted int `json:"not_attempted"`
	Failed       int `json:"failed"`
	Unreadable   int `json:"unreadable"`
	// FailedCodes counts failed publishes by their final code.
	FailedCodes map[string]int `json:"failed_codes"`
	// HeldRate is Held over Eligible.
	HeldRate *float64 `json:"held_rate"`
}

// ReportCI covers attempts that waited for CI: the latest attempt of a
// publish-eligible job whose definition waits for CI and whose publish
// reached the CI wait (a proof record exists). Every stop code, round, and
// re-run counts once per attempt, across `publish` and `publish_history`.
type ReportCI struct {
	Waited int `json:"waited"`
	// FirstPassGreen: clean CI-green accepts with no repair round and no
	// re-run. A pass after a re-run is flaky, never counted here (R6).
	FirstPassGreen int             `json:"first_pass_green"`
	Repair         ReportCIRepair  `json:"repair"`
	Reruns         ReportCIReruns  `json:"reruns"`
	Revisit        ReportCIRevisit `json:"revisit"`
}

// ReportCIRepair is CI repair (publish.ci.on_fail) over waited attempts.
type ReportCIRepair struct {
	// Entered: attempts with a pushed round on the ledger or any round in a
	// publish summary.
	Entered int `json:"entered"`
	// EntryRate is Entered over Waited.
	EntryRate *float64 `json:"entry_rate"`
	// Succeeded: entered attempts whose repair ended with CI green — the
	// summary that carries the rounds is published. A later publish-only
	// retry that goes green does not make a failed repair a success.
	Succeeded int `json:"succeeded"`
	// SuccessRate is Succeeded over Entered.
	SuccessRate *float64 `json:"success_rate"`
	// Rounds counts pushed rounds on the ledger (publish_ci_repairs).
	Rounds int `json:"rounds"`
	// StopCodes counts the code each unsuccessful repair stopped on.
	StopCodes map[string]int `json:"stop_codes"`
}

// ReportCIReruns reads the `ci_reruns` entries of publish summaries.
type ReportCIReruns struct {
	// Attempts: waited attempts with at least one re-run.
	Attempts int `json:"attempts"`
	// Reruns: re-run entries, deduplicated per attempt by their number.
	Reruns int `json:"reruns"`
	// FlakyPasses: attempts with a re-run whose outcome is `passed`.
	FlakyPasses int `json:"flaky_passes"`
}

// ReportCIRevisit carries KTD1's two revisit signals, both over Waited.
type ReportCIRevisit struct {
	// CITimeouts: attempts with any publish summary ending ci_timeout.
	CITimeouts    int      `json:"ci_timeouts"`
	CITimeoutRate *float64 `json:"ci_timeout_rate"`
	// RetryStillRed: attempts whose publish summaries, oldest first, show a
	// red summary directly followed by a publish-only retry that was still
	// red. Red means CI failed: code ci_failed or a failed check recorded.
	RetryStillRed     int      `json:"retry_still_red"`
	RetryStillRedRate *float64 `json:"retry_still_red_rate"`
}

// ReportSpend sums agent_end cost over every attempt of every job in the
// window. Every job lands in exactly one bucket, so the buckets add up to
// the totals.
type ReportSpend struct {
	TotalUSD       float64 `json:"total_usd"`
	UnmeteredSends int     `json:"unmetered_sends"`
	// PerCleanAcceptUSD is TotalUSD over Accepts.Clean: what one clean
	// accept costs once failures, holds, and person-fixed work are paid for.
	// Those never enter the denominator.
	PerCleanAcceptUSD *float64                     `json:"per_clean_accept_usd"`
	ByOutcome         map[string]ReportSpendBucket `json:"by_outcome"`
}

// ReportSpendBucket is one outcome's share of the spend.
type ReportSpendBucket struct {
	Jobs           int     `json:"jobs"`
	CostUSD        float64 `json:"cost_usd"`
	UnmeteredSends int     `json:"unmetered_sends"`
}

// Spend buckets. Every job in the window is in exactly one.
const (
	spendClean       = "clean_accept"
	spendPersonFixed = "person_fixed"
	spendUnverified  = "accepted_unverified"
	spendHeld        = "held"
	spendUnpublished = "unpublished"
	spendFailed      = "failed"
	spendCancelled   = "cancelled"
)

var spendBuckets = []string{spendClean, spendPersonFixed, spendUnverified,
	spendHeld, spendUnpublished, spendFailed, spendCancelled}

// reportJob is one job in the window with everything the report reads.
type reportJob struct {
	id        string
	state     string
	runID     string
	snapshot  string
	attemptID string
	result    string
	proofRef  string
	ciRef     string
	hasCI     bool
	// pushedHeads are the heads jig pushed: the proof ref and every round's
	// head_after.
	pushedHeads map[string]bool
	rounds      int
	cost        float64
	unmetered   int
}

// reportSummary is the part of a publish summary the report reads. Field
// names are the worker's (PublishSummary, CIRepairSummary) plus U4's pinned
// `ci_reruns`.
type reportSummary struct {
	State      string `json:"state"`
	Code       string `json:"code"`
	CIFailures []struct {
		Verdict string `json:"verdict"`
	} `json:"ci_failures"`
	CIRepairs []struct {
		Round   int    `json:"round"`
		Outcome string `json:"outcome"`
	} `json:"ci_repairs"`
	CIReruns []struct {
		Attempt int    `json:"attempt"`
		Outcome string `json:"outcome"`
	} `json:"ci_reruns"`
}

// red reports whether this summary ended with CI red.
func (s reportSummary) red() bool {
	if s.State == "published" {
		return false
	}
	if s.Code == "ci_failed" {
		return true
	}
	for _, failure := range s.CIFailures {
		if failure.Verdict == "fail" {
			return true
		}
	}
	return false
}

// reportResult is one latest-attempt result as the report reads it.
type reportResult struct {
	// publish is the current value: a summary, or one of the engine's
	// markers.
	marker  string
	current *reportSummary
	// summaries are publish_history (oldest first) then the current summary.
	summaries  []reportSummary
	unreadable bool
}

func readReportResult(raw string) reportResult {
	var read reportResult
	var document struct {
		Publish        json.RawMessage   `json:"publish"`
		PublishHistory []json.RawMessage `json:"publish_history"`
	}
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		read.unreadable = true
		return read
	}
	for _, entry := range document.PublishHistory {
		summary, ok := readSummary(entry)
		if !ok {
			read.unreadable = true
			continue
		}
		read.summaries = append(read.summaries, summary)
	}
	var marker string
	switch {
	case len(document.Publish) == 0:
		read.unreadable = true
	case json.Unmarshal(document.Publish, &marker) == nil:
		if marker != "not_attempted" && marker != "held" {
			read.unreadable = true
			break
		}
		read.marker = marker
	default:
		summary, ok := readSummary(document.Publish)
		if !ok {
			read.unreadable = true
			break
		}
		read.current = &summary
		read.summaries = append(read.summaries, summary)
	}
	return read
}

func readSummary(raw json.RawMessage) (reportSummary, bool) {
	var summary reportSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return summary, false
	}
	switch summary.State {
	case "published", "held", "failed":
		return summary, true
	}
	return summary, false
}

// Report computes the quality report over [since, until).
func (s *Store) Report(ctx context.Context, since, until time.Time) (Report, error) {
	report := Report{
		Since: since.UTC(), Until: until.UTC(), ObservedAt: s.now().UTC(),
		Publish: ReportPublish{FailedCodes: map[string]int{}},
		CI:      ReportCI{Repair: ReportCIRepair{StopCodes: map[string]int{}}},
		Spend:   ReportSpend{ByOutcome: map[string]ReportSpendBucket{}},
	}
	for _, bucket := range spendBuckets {
		report.Spend.ByOutcome[bucket] = ReportSpendBucket{}
	}
	if !since.Before(until) {
		return report, invalid("invalid_report_window", "since must be before until")
	}
	jobs, err := s.reportJobs(ctx, since.UnixMilli(), until.UnixMilli())
	if err != nil {
		return report, err
	}
	specs := map[string]*protocol.DefinitionSpec{}
	for _, job := range jobs {
		spec, parsed := specs[job.runID]
		if !parsed {
			spec, _ = protocol.ParseDefinition([]byte(job.snapshot))
			specs[job.runID] = spec
		}
		report.add(job, spec)
	}
	report.finish()
	return report, nil
}

// add folds one job into the report.
func (r *Report) add(job *reportJob, spec *protocol.DefinitionSpec) {
	r.Jobs.Total++
	r.Spend.TotalUSD += job.cost
	r.Spend.UnmeteredSends += job.unmetered
	bucket := ""
	switch job.state {
	case protocol.JobFailed:
		r.Jobs.Failed++
		bucket = spendFailed
	case protocol.JobCancelled:
		r.Jobs.Cancelled++
		bucket = spendCancelled
	case protocol.JobAccepted, protocol.JobAcceptedUnpublished:
		bucket = r.addPublished(job, spec)
	}
	spent := r.Spend.ByOutcome[bucket]
	spent.Jobs++
	spent.CostUSD += job.cost
	spent.UnmeteredSends += job.unmetered
	r.Spend.ByOutcome[bucket] = spent
}

// addPublished classifies one accepted or accepted_unpublished job and
// returns its spend bucket.
func (r *Report) addPublished(job *reportJob, spec *protocol.DefinitionSpec) string {
	r.Publish.Eligible++
	result := readReportResult(job.result)
	if result.unreadable {
		r.UnreadableResults++
	}
	waits := spec != nil && spec.WaitsForCI()
	bucket := ""
	if job.state == protocol.JobAccepted {
		r.Jobs.Accepted++
		r.Publish.Published++
		switch {
		case spec == nil || (waits && !job.hasCI):
			r.Accepts.Unverified++
			bucket = spendUnverified
		case !waits:
			r.Accepts.Clean++
			r.Accepts.NoCIWait++
			bucket = spendClean
		case job.pushedHeads[job.ciRef]:
			r.Accepts.Clean++
			r.Accepts.CIGreen++
			bucket = spendClean
		default:
			r.Accepts.PersonFixed++
			bucket = spendPersonFixed
		}
	} else {
		r.Jobs.AcceptedUnpublished++
		bucket = spendUnpublished
		switch {
		case result.marker == "held" || (result.current != nil && result.current.State == "held"):
			r.Publish.Held++
			bucket = spendHeld
		case result.marker == "not_attempted":
			r.Publish.NotAttempted++
		case result.current != nil && result.current.State == "failed":
			r.Publish.Failed++
			code := result.current.Code
			if code == "" {
				code = "unknown"
			}
			r.Publish.FailedCodes[code]++
		default:
			r.Publish.Unreadable++
		}
	}
	if waits && job.proofRef != "" {
		r.addWaited(job, result, bucket == spendClean)
	}
	return bucket
}

// addWaited folds one attempt that waited for CI into the CI section.
func (r *Report) addWaited(job *reportJob, result reportResult, cleanGreen bool) {
	r.CI.Waited++

	// Repair: the last summary that carries rounds is the repair's own
	// outcome; a later retry's summary carries none.
	var repair *reportSummary
	for index := range result.summaries {
		if len(result.summaries[index].CIRepairs) > 0 {
			repair = &result.summaries[index]
		}
	}
	entered := job.rounds > 0 || repair != nil
	if entered {
		r.CI.Repair.Entered++
		r.CI.Repair.Rounds += job.rounds
		switch {
		case repair != nil && repair.State == "published":
			r.CI.Repair.Succeeded++
		case repair != nil && repair.Code != "":
			r.CI.Repair.StopCodes[repair.Code]++
		default:
			r.CI.Repair.StopCodes["unknown"]++
		}
	}

	reruns := map[int]string{}
	timedOut := false
	stillRed := false
	for index, summary := range result.summaries {
		for _, rerun := range summary.CIReruns {
			reruns[rerun.Attempt] = rerun.Outcome
		}
		if summary.Code == "ci_timeout" {
			timedOut = true
		}
		if index > 0 && summary.red() && result.summaries[index-1].red() {
			stillRed = true
		}
	}
	flaky := false
	for _, outcome := range reruns {
		if outcome == "passed" {
			flaky = true
		}
	}
	if len(reruns) > 0 {
		r.CI.Reruns.Attempts++
		r.CI.Reruns.Reruns += len(reruns)
	}
	if flaky {
		r.CI.Reruns.FlakyPasses++
	}
	if timedOut {
		r.CI.Revisit.CITimeouts++
	}
	if stillRed {
		r.CI.Revisit.RetryStillRed++
	}
	if cleanGreen && job.state == protocol.JobAccepted && !entered && len(reruns) == 0 {
		r.CI.FirstPassGreen++
	}
}

func (r *Report) finish() {
	r.Publish.HeldRate = ratio(r.Publish.Held, r.Publish.Eligible)
	r.CI.Repair.EntryRate = ratio(r.CI.Repair.Entered, r.CI.Waited)
	r.CI.Repair.SuccessRate = ratio(r.CI.Repair.Succeeded, r.CI.Repair.Entered)
	r.CI.Revisit.CITimeoutRate = ratio(r.CI.Revisit.CITimeouts, r.CI.Waited)
	r.CI.Revisit.RetryStillRedRate = ratio(r.CI.Revisit.RetryStillRed, r.CI.Waited)
	if r.Accepts.Clean > 0 {
		per := r.Spend.TotalUSD / float64(r.Accepts.Clean)
		r.Spend.PerCleanAcceptUSD = &per
	}
}

func ratio(part, whole int) *float64 {
	if whole == 0 {
		return nil
	}
	value := float64(part) / float64(whole)
	return &value
}

// reportWindowJobs selects the terminal jobs in the window; every report
// query joins it so all of them read the same set.
const reportWindowJobs = `
	SELECT id FROM jobs
	WHERE state IN ('accepted', 'accepted_unpublished', 'failed', 'cancelled')
	  AND updated_at >= ? AND updated_at < ?`

// reportLatestAttempts is each window job's latest attempt.
const reportLatestAttempts = `
	SELECT a.id FROM attempts a
	WHERE a.job_id IN (` + reportWindowJobs + `)
	  AND a.attempt_number = (SELECT MAX(attempt_number) FROM attempts WHERE job_id = a.job_id)`

// reportJobs reads every job in the window with its latest attempt, its
// publish ledger, its rounds, and its spend over all attempts.
func (s *Store) reportJobs(ctx context.Context, since, until int64) ([]*reportJob, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.id, j.state, r.id, r.snapshot, COALESCE(a.id, ''), COALESCE(a.result, '')
		FROM jobs j
		JOIN runs r ON r.id = j.run_id
		LEFT JOIN attempts a ON a.job_id = j.id
		 AND a.attempt_number = (SELECT MAX(attempt_number) FROM attempts WHERE job_id = j.id)
		WHERE j.id IN (`+reportWindowJobs+`)
		ORDER BY j.id
	`, since, until)
	if err != nil {
		return nil, unavailable(err)
	}
	jobs := []*reportJob{}
	byID := map[string]*reportJob{}
	byAttempt := map[string]*reportJob{}
	for rows.Next() {
		job := &reportJob{pushedHeads: map[string]bool{}}
		if err := rows.Scan(&job.id, &job.state, &job.runID, &job.snapshot,
			&job.attemptID, &job.result); err != nil {
			rows.Close()
			return nil, unavailable(err)
		}
		jobs = append(jobs, job)
		byID[job.id] = job
		if job.attemptID != "" {
			byAttempt[job.attemptID] = job
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, unavailable(err)
	}
	rows.Close()

	if err := s.reportLedger(ctx, since, until, byAttempt); err != nil {
		return nil, err
	}
	if err := s.reportSpend(ctx, since, until, byID); err != nil {
		return nil, err
	}
	return jobs, nil
}

// reportLedger reads the proof and ci records and the pushed rounds of each
// latest attempt.
func (s *Store) reportLedger(ctx context.Context, since, until int64, byAttempt map[string]*reportJob) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT attempt_id, step, remote_ref FROM publish_records
		WHERE step IN (?, ?) AND attempt_id IN (`+reportLatestAttempts+`)
	`, protocol.PublishStepProof, protocol.PublishStepCI, since, until)
	if err != nil {
		return unavailable(err)
	}
	for rows.Next() {
		var attemptID, step, ref string
		if err := rows.Scan(&attemptID, &step, &ref); err != nil {
			rows.Close()
			return unavailable(err)
		}
		job := byAttempt[attemptID]
		if job == nil {
			continue
		}
		if step == protocol.PublishStepProof {
			job.proofRef = ref
			job.pushedHeads[ref] = true
		} else {
			job.ciRef = ref
			job.hasCI = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return unavailable(err)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `
		SELECT attempt_id, head_after FROM publish_ci_repairs
		WHERE attempt_id IN (`+reportLatestAttempts+`)
	`, since, until)
	if err != nil {
		return unavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var attemptID, head string
		if err := rows.Scan(&attemptID, &head); err != nil {
			return unavailable(err)
		}
		if job := byAttempt[attemptID]; job != nil {
			job.rounds++
			job.pushedHeads[head] = true
		}
	}
	if err := rows.Err(); err != nil {
		return unavailable(err)
	}
	return nil
}

// reportSpend sums agent_end cost and unmetered sends over every attempt of
// each window job. The stored payload is the whole marshalled event, so the
// agent's numbers sit under $.payload. A missing field (an event from before
// the field existed) adds nothing.
func (s *Store) reportSpend(ctx context.Context, since, until int64, byID map[string]*reportJob) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.job_id,
		       COALESCE(SUM(json_extract(CAST(e.payload AS TEXT), '$.payload.cost')), 0),
		       CAST(COALESCE(SUM(json_extract(CAST(e.payload AS TEXT), '$.payload.unmetered_sends')), 0) AS INTEGER)
		FROM events e JOIN attempts a ON a.id = e.attempt_id
		WHERE e.type = ? AND json_valid(CAST(e.payload AS TEXT))
		  AND a.job_id IN (`+reportWindowJobs+`)
		GROUP BY a.job_id
	`, protocol.EventAgentEnd, since, until)
	if err != nil {
		return unavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var jobID string
		var cost float64
		var unmetered int
		if err := rows.Scan(&jobID, &cost, &unmetered); err != nil {
			return unavailable(err)
		}
		if job := byID[jobID]; job != nil {
			job.cost = cost
			job.unmetered = unmetered
		}
	}
	if err := rows.Err(); err != nil {
		return unavailable(err)
	}
	return nil
}

// ---- HTTP surface ----------------------------------------------------------

// registerReportRoutes attaches the report. It is a GET with no side
// effects, so it takes no mutation gate (the read-surface rule in ingest.go).
func (a *API) registerReportRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/report", a.report)
}

// report answers GET /api/report?since=<RFC3339>&until=<RFC3339>. until
// defaults to now and since to seven days before until.
func (a *API) report(w http.ResponseWriter, r *http.Request) {
	until, err := queryTime(r, "until", a.store.now())
	if err != nil {
		writeError(w, err)
		return
	}
	since, err := queryTime(r, "since", until.Add(-defaultReportWindow))
	if err != nil {
		writeError(w, err)
		return
	}
	report, err := a.store.Report(r.Context(), since, until)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// queryTime reads one RFC3339 query parameter. A malformed value is a 400
// naming the parameter, never a silent default (the queryInt rule).
func queryTime(r *http.Request, name string, fallback time.Time) (time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, invalid("invalid_query_parameter",
			fmt.Sprintf("%s must be an RFC3339 timestamp", name))
	}
	return value, nil
}
