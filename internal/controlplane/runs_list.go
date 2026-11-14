package controlplane

import (
	"context"
	"database/sql"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// RunListItem is one row of GET /api/runs: the run exactly as it was listed
// before, plus a summary of each of its jobs. The summary exists because the
// jobs list lit every row with two more requests (GET /api/runs/:id, then
// GET /api/jobs/:id) — 84 requests for 40 jobs, and rows that flickered
// unlit until both returned (U8 read surface).
//
// The run is embedded so its JSON fields stay flat and unchanged: a UI that
// does not know job_summaries reads the list as it always did.
type RunListItem struct {
	protocol.Run
	JobSummaries []JobSummary `json:"job_summaries"`
}

// JobSummary is the slice of JobDetail a list row needs: the job, the frozen
// chain and publish policy, and the latest attempt with that attempt's
// publish records. It reuses JobDetail's field names and types on purpose —
// the UI folds stations from (stations, delivery, attempt, publish) with one
// function, and a second server-side vocabulary for station states would
// drift from it. Earlier attempts and the retained worktree stay on the job
// page; a row shows only where the job stands now.
type JobSummary struct {
	Job      protocol.Job `json:"job"`
	Stations []Station    `json:"stations,omitempty"`
	Delivery *Delivery    `json:"delivery,omitempty"`
	// AttemptCount says how many attempts the job has had, so a row can mark
	// a retried job without the attempt history.
	AttemptCount  int                      `json:"attempt_count"`
	LatestAttempt *protocol.Attempt        `json:"latest_attempt,omitempty"`
	Publish       []protocol.PublishRecord `json:"publish"`
}

// RunList reads every run, newest first, with its job summaries. The query
// count is fixed — runs, jobs, latest attempts, their publish records —
// whatever the number of runs: the point of the summary is to end a fetch
// per row, and a query per row would only move that loop server-side.
func (s *Store) RunList(ctx context.Context) ([]RunListItem, error) {
	runs, err := s.Runs(ctx)
	if err != nil {
		return nil, err
	}
	jobsByRun, err := s.allJobsByRun(ctx)
	if err != nil {
		return nil, err
	}
	latest, counts, err := s.latestAttempts(ctx)
	if err != nil {
		return nil, err
	}
	// Only the latest attempt's records: the row's PR link and the publish
	// and CI lamps are all read for that attempt alone.
	records, err := s.publishRecords(ctx, `a.attempt_number = (
		SELECT MAX(x.attempt_number) FROM attempts x WHERE x.job_id = a.job_id)`)
	if err != nil {
		return nil, err
	}
	publishByAttempt := map[string][]protocol.PublishRecord{}
	for _, record := range records {
		publishByAttempt[record.AttemptID] = append(publishByAttempt[record.AttemptID], record)
	}

	items := make([]RunListItem, 0, len(runs))
	for _, run := range runs {
		item := RunListItem{Run: run, JobSummaries: []JobSummary{}}
		jobs := jobsByRun[run.ID]
		if len(jobs) > 0 {
			// The chain is per run; parse the snapshot once, not per job.
			stations, delivery := stationsFromSnapshot(run.Snapshot)
			for _, job := range jobs {
				summary := JobSummary{
					Job:          job,
					Stations:     stations,
					Delivery:     delivery,
					AttemptCount: counts[job.ID],
					Publish:      []protocol.PublishRecord{},
				}
				if attempt, ok := latest[job.ID]; ok {
					summary.LatestAttempt = &attempt
					if records := publishByAttempt[attempt.ID]; records != nil {
						summary.Publish = records
					}
				}
				item.JobSummaries = append(item.JobSummaries, summary)
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// allJobsByRun reads every job in one query, grouped by run in the creation
// order RunJobs uses.
func (s *Store) allJobsByRun(ctx context.Context) (map[string][]protocol.Job, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs ORDER BY run_id, created_at, id`)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	byRun := map[string][]protocol.Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		byRun[job.RunID] = append(byRun[job.RunID], job)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return byRun, nil
}

// latestAttempts reads each job's highest-numbered attempt and its attempt
// count in one query. UNIQUE(job_id, attempt_number) makes the join pick
// exactly one attempt per job.
func (s *Store) latestAttempts(ctx context.Context) (map[string]protocol.Attempt, map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT latest.attempt_count, `+attemptColumns+`
		FROM attempts JOIN (
			SELECT job_id AS latest_job, MAX(attempt_number) AS latest_number, COUNT(*) AS attempt_count
			FROM attempts GROUP BY job_id
		) latest ON latest.latest_job = attempts.job_id AND latest.latest_number = attempts.attempt_number
	`)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	defer rows.Close()
	latest := map[string]protocol.Attempt{}
	counts := map[string]int{}
	for rows.Next() {
		var count int
		attempt, err := scanAttempt(prefixedScanner{rows: rows, prefix: []any{&count}})
		if err != nil {
			return nil, nil, unavailable(err)
		}
		latest[attempt.JobID] = attempt
		counts[attempt.JobID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, nil, unavailable(err)
	}
	return latest, counts, nil
}

// prefixedScanner lets scanAttempt read a row that carries extra leading
// columns, so the attempt column list and its scan stay defined once.
type prefixedScanner struct {
	rows   *sql.Rows
	prefix []any
}

func (p prefixedScanner) Scan(dest ...any) error {
	return p.rows.Scan(append(append([]any{}, p.prefix...), dest...)...)
}
