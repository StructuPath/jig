// report_test.go — `jig report` (plan 2026-09-28-001, U3) against a live
// `jig serve` whose ledger is seeded row by row: the prose names every
// section and both caveat lines, --json is the API's own object, --since
// takes a duration or a timestamp, and the failures exit 2 or 1.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/internal/protocol"
)

// startReportServe seeds a ledger and serves it. Three terminal jobs: a
// clean accept under a definition that does not wait for CI and a failure
// with unmetered sends, both updated an hour ago, and a failure updated 30
// days ago that only a wide window sees.
func startReportServe(t *testing.T) (string, func()) {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "jig.db")
	store, err := controlplane.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(stockDefinition("smoke.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	now := time.Now()
	exec(`INSERT INTO definitions(id, name, generation, source, created_at, updated_at)
		VALUES ('def-smoke', 'smoke', 1, ?, ?, ?)`, string(source), now.UnixMilli(), now.UnixMilli())
	exec(`INSERT INTO runs(id, definition_id, definition_generation, snapshot, parameters, targets, state, created_at, updated_at)
		VALUES ('run-smoke', 'def-smoke', 1, ?, '{}', '[]', 'active', ?, ?)`,
		string(source), now.UnixMilli(), now.UnixMilli())

	seq := int64(0)
	job := func(id, state, attemptState, result string, updated time.Time, cost float64, unmetered int) string {
		t.Helper()
		exec(`INSERT INTO jobs(id, run_id, repository, base_sha, state, created_at, updated_at)
			VALUES (?, 'run-smoke', ?, ?, ?, ?, ?)`, id, "github.com/example/"+id,
			strings.Repeat("a", 40), state, updated.UnixMilli(), updated.UnixMilli())
		attemptID := id + "-attempt-1"
		var stored any
		if result != "" {
			stored = result
		}
		exec(`INSERT INTO attempts(id, job_id, attempt_number, state, result, created_at)
			VALUES (?, ?, 1, ?, ?, ?)`, attemptID, id, attemptState, stored, updated.UnixMilli())
		payload, _ := json.Marshal(map[string]any{
			"cost": cost, "tokens": 100, "sends": 1, "unmetered_sends": unmetered})
		event, _ := json.Marshal(protocol.Event{
			Seq: seq, Type: protocol.EventAgentEnd, Phase: "scout", Name: "scout", Payload: payload})
		exec(`INSERT INTO events(attempt_id, seq, type, phase, payload, payload_bytes, server_time)
			VALUES (?, ?, ?, 'scout', ?, ?, ?)`, attemptID, seq, protocol.EventAgentEnd, event,
			len(event), updated.UnixMilli())
		seq++
		return attemptID
	}
	hourAgo := now.Add(-time.Hour)
	clean := job("job-clean", protocol.JobAccepted, protocol.AttemptAccepted,
		`{"publish":{"state":"published","branch":"jig/job-clean"}}`, hourAgo, 0.75, 0)
	exec(`INSERT INTO publish_records(attempt_id, step, branch, remote_ref, pr_url, completed_at)
		VALUES (?, ?, 'jig/job-clean', ?, '', ?)`, clean, protocol.PublishStepProof,
		strings.Repeat("b", 40), hourAgo.UnixMilli())
	job("job-failed", protocol.JobFailed, protocol.AttemptFailed, "", hourAgo, 0.25, 2)
	job("job-old", protocol.JobFailed, protocol.AttemptFailed, "", now.Add(-30*24*time.Hour), 4, 0)

	return startServe(t, dataDir, "--no-github-poll")
}

func jigReport(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := reportCommand(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// reportObject decodes a report and drops observed_at, the one field that
// differs between two reads of the same window.
func reportObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode report %q: %v", body, err)
	}
	if _, found := object["observed_at"]; !found {
		t.Fatalf("report has no observed_at: %s", body)
	}
	delete(object, "observed_at")
	return object
}

func TestReportProseNamesEverySectionAndBothCaveats(t *testing.T) {
	base, stop := startReportServe(t)
	defer stop()

	code, stdout, stderr := jigReport(t, "--server", base)
	if code != exitAccepted {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitAccepted, stderr)
	}
	for _, want := range []string{
		"\nJobs\n", "\nAccepts\n", "\nPublish\n", "\nCI repair\n", "\nRe-runs\n", "\nSpend\n",
		// The R4 caveat, one line.
		"Not seen: jig does not watch CI after accept, so there is no post-merge CI rate; " +
			"person-fixed accepts and accepts that did not wait for CI are counted separately.\n",
		// The seeded failure's two killed sends, one line.
		"Spend undercounts: 2 unmetered send(s) were killed before returning a cost",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("prose is missing %q\n---\n%s", want, stdout)
		}
	}
	for _, want := range []string{
		// Nothing waited for CI, so a CI rate is n/a, never 0%.
		`(?m)^  entry rate +n/a$`,
		// $1.00 over the one clean accept; the 30-day-old job is outside 7d.
		`(?m)^  per clean accept +\$1\.00$`,
		`(?m)^  clean +1$`,
	} {
		if !regexp.MustCompile(want).MatchString(stdout) {
			t.Errorf("prose does not match %q\n---\n%s", want, stdout)
		}
	}
}

func TestReportJSONIsTheAPIObject(t *testing.T) {
	base, stop := startReportServe(t)
	defer stop()
	since, until := "2026-01-01T00:00:00Z", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	code, stdout, stderr := jigReport(t, "--server", base, "--json", "--since", since, "--until", until)
	if code != exitAccepted {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitAccepted, stderr)
	}
	response, err := http.Get(base + "/api/report?since=" + since + "&until=" + until)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	api, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reportObject(t, []byte(stdout)), reportObject(t, api); !reflect.DeepEqual(got, want) {
		t.Errorf("--json =\n%v\nwant GET /api/report =\n%v", got, want)
	}
}

func TestReportSinceTakesADurationOrATimestamp(t *testing.T) {
	base, stop := startReportServe(t)
	defer stop()

	window := func(args ...string) controlplane.Report {
		t.Helper()
		code, stdout, stderr := jigReport(t, append([]string{"--server", base, "--json"}, args...)...)
		if code != exitAccepted {
			t.Fatalf("%v: exit = %d (stderr: %s)", args, code, stderr)
		}
		var report controlplane.Report
		if err := json.Unmarshal([]byte(stdout), &report); err != nil {
			t.Fatal(err)
		}
		return report
	}

	week := window("--since", "7d")
	if drift := time.Since(week.Since) - 7*24*time.Hour; drift < 0 || drift > time.Minute {
		t.Errorf("--since 7d sent since = %s, want seven days before now", week.Since)
	}
	if week.Jobs.Total != 2 {
		t.Errorf("--since 7d: jobs = %d, want 2 (the 30-day-old job is outside)", week.Jobs.Total)
	}
	if recent := window("--since", "90m"); recent.Jobs.Total != 2 {
		t.Errorf("--since 90m: jobs = %d, want 2", recent.Jobs.Total)
	}
	if narrow := window("--since", "30m"); narrow.Jobs.Total != 0 {
		t.Errorf("--since 30m: jobs = %d, want 0 (both recent jobs are an hour old)", narrow.Jobs.Total)
	}

	since := time.Now().Add(-40 * 24 * time.Hour).UTC().Truncate(time.Second)
	wide := window("--since", since.Format(time.RFC3339))
	if !wide.Since.Equal(since) {
		t.Errorf("--since %s sent since = %s", since.Format(time.RFC3339), wide.Since)
	}
	if wide.Jobs.Total != 3 {
		t.Errorf("--since 40 days ago: jobs = %d, want 3", wide.Jobs.Total)
	}
}

func TestReportRejectsAnUnparseableWindowWithUsage(t *testing.T) {
	for _, args := range [][]string{
		{"--since", "last week"},
		{"--since", "7"},
		{"--since", "-7d"},
		{"--until", "7d"},
		{"stray"},
	} {
		// No server is contacted: the URL is never reached.
		code, _, stderr := jigReport(t, append([]string{"--server", "http://127.0.0.1:1"}, args...)...)
		if code != exitUsage {
			t.Errorf("%v: exit = %d, want %d", args, code, exitUsage)
		}
		if !strings.Contains(stderr, "Usage: jig report") {
			t.Errorf("%v: stderr carries no usage:\n%s", args, stderr)
		}
	}
}

func TestReportExitsOneWhenTheServerIsUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()

	code, stdout, stderr := jigReport(t, "--server", "http://"+address)
	if code != exitInfraFailed {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitInfraFailed, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, "jig report: GET /api/report") {
		t.Errorf("stderr does not name the failed request: %s", stderr)
	}
}
