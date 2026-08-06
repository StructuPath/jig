// schedule.go — unattended admission: triggers, occurrences, and the
// commit-then-dispatch protocol they share (U6, R13).
//
// R13's demand is structural idempotency, and the structure is this: an
// occurrence carrying a UNIQUE request key is committed BEFORE the run
// exists, and the run is created in the SAME transaction that marks the
// occurrence dispatched. Those two sentences carry all four failure modes:
//
//   - Two polls observing one issue collide on the request key; the second
//     writes nothing.
//   - A crash after occurrence-commit and before dispatch leaves a `pending`
//     row and no run; startup re-drives it and produces exactly one run.
//   - A crash mid-dispatch rolls back the run WITH the dispatched mark,
//     because they are one transaction — recovery cannot double-admit
//     something it can see was never admitted.
//   - A lost HTTP response cannot duplicate anything, because admission is
//     driven by durable rows, never by a caller's retry.
//
// Schedules add one more rule, the one that makes downtime survivable: a
// trigger's memory of the future is a SINGLE stored instant. On wake, that
// one overdue instant is admitted and the cursor jumps to the next future
// match. A backlog is never enumerated because it was never written, so
// "never catch up" is a property of the schema rather than a rule the
// scheduler has to remember to obey.
package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// Trigger kinds. A trigger is either a clock or a GitHub query; there is no
// third kind in v1 (KTD7 rules out webhooks on a loopback server).
const (
	TriggerSchedule          = "schedule"
	TriggerGitHubIssue       = "github_issue"
	TriggerGitHubPullRequest = "github_pull_request"
)

// Occurrence states. `pending` is committed-not-yet-run, `dispatching` is
// reserved-by-a-dispatcher, `dispatched` carries a run, and `skipped` and
// `failed` are the two ways a firing ends without one.
const (
	OccurrencePending     = "pending"
	OccurrenceDispatching = "dispatching"
	OccurrenceDispatched  = "dispatched"
	OccurrenceSkipped     = "skipped"
	OccurrenceFailed      = "failed"
)

// TriggerConfig is a trigger's frozen configuration. One flat shape covers
// both kinds because the alternative — a per-kind union decoded from a raw
// JSON blob — hides which fields a given kind ignores; validation names them
// explicitly instead.
//
// Instructions is the TRUSTED half of the prompt, authored by the operator
// when the trigger is saved. Issue and pull-request text never lands here: it
// travels as untrusted context (prompt.go), which is the whole point of
// splitting the two halves at admission.
type TriggerConfig struct {
	// Instructions is what the operator wants done on every firing.
	Instructions string `json:"instructions"`
	// Parameters are extra frozen run parameters. It may not contain
	// "prompt": the composed prompt occupies that name, and passing both is
	// exactly the ambiguity InvokeDefinition refuses.
	Parameters map[string]string `json:"parameters,omitempty"`

	// ---- schedule ----------------------------------------------------------

	// Cron is a five-field expression, Timezone an IANA name (never "Local").
	Cron     string `json:"cron,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	// Targets are the repositories a scheduled run fans out to.
	Targets []InvocationTarget `json:"targets,omitempty"`

	// ---- github ------------------------------------------------------------

	// Repository is the single repository a GitHub trigger watches AND runs
	// against — never a URL read out of a ticket (R17).
	Repository string `json:"repository,omitempty"`
	// State selects which issues or pull requests count as matches.
	State string `json:"state,omitempty"`
	// RequiredLabels narrows matches; every named label must be present.
	RequiredLabels []string `json:"required_labels,omitempty"`
	// BaseBranches and IncludeDrafts narrow pull-request matches.
	BaseBranches  []string `json:"base_branches,omitempty"`
	IncludeDrafts bool     `json:"include_drafts,omitempty"`
	// PollIntervalSeconds is how often `gh` may be spent on this trigger.
	PollIntervalSeconds int `json:"poll_interval_seconds,omitempty"`
}

// TriggerInput is the create/update body.
type TriggerInput struct {
	Name         string        `json:"name"`
	DefinitionID string        `json:"definition_id"`
	Kind         string        `json:"kind"`
	Config       TriggerConfig `json:"config"`
}

// Trigger is one standing admission intent. Counters are surfaced rather than
// derived: SkippedCount is R13's "surfaces a counter", and it is the only
// place an operator sees that a schedule has been firing into a run that never
// finishes.
type Trigger struct {
	ID             string        `json:"id"`
	DefinitionID   string        `json:"definition_id"`
	Name           string        `json:"name"`
	Kind           string        `json:"kind"`
	Enabled        bool          `json:"enabled"`
	Config         TriggerConfig `json:"config"`
	NextDueAt      *time.Time    `json:"next_due_at,omitempty"`
	NextPollAt     *time.Time    `json:"next_poll_at,omitempty"`
	LastCheckedAt  *time.Time    `json:"last_checked_at,omitempty"`
	AdmittedCount  int           `json:"admitted_count"`
	SkippedCount   int           `json:"skipped_count"`
	DiagnosticCode string        `json:"diagnostic_code,omitempty"`
	Diagnostic     string        `json:"diagnostic,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// Occurrence is the durable record of one firing. Source freezes what was
// observed, so the row still explains itself after the issue is edited,
// relabelled, or closed.
type Occurrence struct {
	ID          string          `json:"id"`
	TriggerID   string          `json:"trigger_id"`
	RequestKey  string          `json:"request_key"`
	State       string          `json:"state"`
	RunID       string          `json:"run_id,omitempty"`
	Source      json.RawMessage `json:"source"`
	ScheduledAt *time.Time      `json:"scheduled_at,omitempty"`
	Diagnostic  string          `json:"diagnostic,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// occurrenceSource is the frozen observation an occurrence dispatches from.
// It is the only input to prompt composition at dispatch time — dispatch
// never re-reads GitHub, so a run always describes the world as it was when
// the trigger matched it.
type occurrenceSource struct {
	Kind        string   `json:"kind"`
	ScheduledAt string   `json:"scheduled_at,omitempty"`
	Cron        string   `json:"cron,omitempty"`
	Timezone    string   `json:"timezone,omitempty"`
	Repository  string   `json:"repository,omitempty"`
	Number      int      `json:"number,omitempty"`
	Title       string   `json:"title,omitempty"`
	URL         string   `json:"url,omitempty"`
	Body        string   `json:"body,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	BaseBranch  string   `json:"base_branch,omitempty"`
	HeadSHA     string   `json:"head_sha,omitempty"`
}

// ---- trigger authoring -----------------------------------------------------

// CreateTrigger validates and stores a trigger. It is ALWAYS created
// disabled, whatever the caller intended: a trigger that starts firing the
// instant it is saved admits work before the operator has read back what they
// just wrote, and the cheapest place to stop that is the constructor.
// Enabling is a separate, explicit action — and the moment a schedule's
// cursor is first computed.
func (s *Store) CreateTrigger(ctx context.Context, input TriggerInput) (Trigger, error) {
	var zero Trigger
	normalized, err := s.validateTriggerInput(ctx, input)
	if err != nil {
		return zero, err
	}
	configJSON, err := marshalTriggerConfig(normalized.Config)
	if err != nil {
		return zero, err
	}
	id, err := newID()
	if err != nil {
		return zero, unavailable(err)
	}
	now := s.now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO triggers(id, definition_id, name, kind, enabled, config, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?)
	`, id, normalized.DefinitionID, normalized.Name, normalized.Kind, configJSON, now, now); err != nil {
		if isConstraintViolation(err) {
			return zero, conflict("trigger_exists",
				"a trigger named "+quote(normalized.Name)+" already exists — update it by id")
		}
		return zero, unavailable(err)
	}
	return s.Trigger(ctx, id)
}

// UpdateTrigger replaces a trigger's definition, kind, and configuration in
// place. An enabled trigger keeps firing across the edit, but its cursors are
// recomputed from now: the stored instant belonged to the old cron, and
// carrying it over would fire the new schedule at the old schedule's time.
func (s *Store) UpdateTrigger(ctx context.Context, triggerID string, input TriggerInput) (Trigger, error) {
	var zero Trigger
	normalized, err := s.validateTriggerInput(ctx, input)
	if err != nil {
		return zero, err
	}
	configJSON, err := marshalTriggerConfig(normalized.Config)
	if err != nil {
		return zero, err
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, unavailable(err)
	}
	defer tx.Rollback()
	var enabled int
	err = tx.QueryRowContext(ctx, `SELECT enabled FROM triggers WHERE id = ?`, triggerID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, ErrNotFound
	}
	if err != nil {
		return zero, unavailable(err)
	}
	nextDue, nextPoll, err := triggerCursors(normalized.Kind, normalized.Config, enabled != 0, now)
	if err != nil {
		return zero, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE triggers
		SET definition_id = ?, name = ?, kind = ?, config = ?,
		    next_due_at = ?, next_poll_at = ?, diagnostic_code = '', diagnostic = '', updated_at = ?
		WHERE id = ?
	`, normalized.DefinitionID, normalized.Name, normalized.Kind, configJSON,
		nextDue, nextPoll, now.UnixMilli(), triggerID); err != nil {
		if isConstraintViolation(err) {
			return zero, conflict("trigger_exists",
				"another trigger is already named "+quote(normalized.Name))
		}
		return zero, unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return zero, unavailable(err)
	}
	return s.Trigger(ctx, triggerID)
}

// SetTriggerEnabled turns a trigger on or off. Enabling computes the cursors;
// disabling clears them, so a disabled trigger has no future — it cannot
// accumulate an overdue instant while it is off, and re-enabling therefore
// starts from now rather than from whenever it was last on (R13).
func (s *Store) SetTriggerEnabled(ctx context.Context, triggerID string, enabled bool) (Trigger, error) {
	var zero Trigger
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, unavailable(err)
	}
	defer tx.Rollback()
	trigger, err := loadTriggerTx(ctx, tx, triggerID)
	if err != nil {
		return zero, err
	}
	nextDue, nextPoll, err := triggerCursors(trigger.Kind, trigger.Config, enabled, now)
	if err != nil {
		return zero, err
	}
	flag := 0
	if enabled {
		flag = 1
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE triggers SET enabled = ?, next_due_at = ?, next_poll_at = ?,
		    diagnostic_code = '', diagnostic = '', updated_at = ?
		WHERE id = ?
	`, flag, nextDue, nextPoll, now.UnixMilli(), triggerID); err != nil {
		return zero, unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return zero, unavailable(err)
	}
	return s.Trigger(ctx, triggerID)
}

// DeleteTrigger removes a disabled trigger and its occurrence history in one
// transaction. It refuses an enabled trigger: deleting something mid-fire is
// how an occurrence ends up orphaned from the trigger that explains it, and
// "disable, look, then delete" costs one extra call.
func (s *Store) DeleteTrigger(ctx context.Context, triggerID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable(err)
	}
	defer tx.Rollback()
	var enabled int
	err = tx.QueryRowContext(ctx, `SELECT enabled FROM triggers WHERE id = ?`, triggerID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return unavailable(err)
	}
	if enabled != 0 {
		return conflict("trigger_enabled", "disable the trigger before deleting it")
	}
	var live int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM occurrences WHERE trigger_id = ? AND state IN ('pending', 'dispatching')
	`, triggerID).Scan(&live); err != nil {
		return unavailable(err)
	}
	if live != 0 {
		return conflict("trigger_has_undispatched_occurrences",
			"the trigger still has undispatched occurrences; let them dispatch before deleting it")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM occurrences WHERE trigger_id = ?`, triggerID); err != nil {
		return unavailable(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM triggers WHERE id = ?`, triggerID); err != nil {
		return unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return unavailable(err)
	}
	return nil
}

// triggerCursors computes the stored future for a trigger in one place: a
// schedule's single next instant and a GitHub trigger's next poll time, or
// nothing at all when it is disabled.
func triggerCursors(kind string, config TriggerConfig, enabled bool, now time.Time) (any, any, error) {
	if !enabled {
		return nil, nil, nil
	}
	if kind != TriggerSchedule {
		// A newly enabled GitHub trigger polls immediately: the operator just
		// asked for it, and the first answer is the one that proves the
		// configuration works.
		return nil, now.UnixMilli(), nil
	}
	schedule, _, _, err := parseCronSchedule(config.Cron, config.Timezone)
	if err != nil {
		return nil, nil, invalid("invalid_cron", err.Error())
	}
	next, err := schedule.Next(now)
	if err != nil {
		return nil, nil, invalid("invalid_cron", err.Error())
	}
	return next.UnixMilli(), nil, nil
}

// validateTriggerInput is the save-time contract. Everything a firing needs
// is checked here — the definition exists, the cron parses, the repositories
// normalize, the prompt halves are unambiguous — so an enabled trigger cannot
// fail for a reason that was knowable when it was saved.
func (s *Store) validateTriggerInput(ctx context.Context, input TriggerInput) (TriggerInput, error) {
	var zero TriggerInput
	normalized := input
	normalized.Name = strings.TrimSpace(input.Name)
	if normalized.Name == "" || len(normalized.Name) > 200 {
		return zero, invalid("invalid_trigger_name", "trigger name is required and at most 200 bytes")
	}
	normalized.DefinitionID = strings.TrimSpace(input.DefinitionID)
	if _, err := s.Definition(ctx, normalized.DefinitionID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return zero, invalid("unknown_definition",
				"no definition with id "+quote(normalized.DefinitionID))
		}
		return zero, err
	}
	switch input.Kind {
	case TriggerSchedule, TriggerGitHubIssue, TriggerGitHubPullRequest:
	default:
		return zero, invalid("invalid_trigger_kind",
			"kind must be one of schedule, github_issue, github_pull_request")
	}
	config, err := validateTriggerConfig(input.Kind, input.Config)
	if err != nil {
		return zero, err
	}
	normalized.Config = config
	return normalized, nil
}

func validateTriggerConfig(kind string, config TriggerConfig) (TriggerConfig, error) {
	var zero TriggerConfig
	config.Instructions = strings.TrimSpace(config.Instructions)
	if config.Instructions == "" {
		return zero, invalid("invalid_instructions",
			"trigger instructions are required: they are the trusted half of every prompt this trigger composes")
	}
	if len(config.Instructions) > protocol.MaxInstructionsBytes {
		return zero, invalid("invalid_instructions", "trigger instructions exceed their byte limit")
	}
	if _, taken := config.Parameters[protocol.PromptParameter]; taken {
		return zero, invalid("ambiguous_prompt",
			"a trigger composes the "+quote(protocol.PromptParameter)+
				" parameter from its instructions and context; it may not also set it directly")
	}
	if kind == TriggerSchedule {
		schedule, canonicalCron, canonicalTimezone, err := parseCronSchedule(config.Cron, config.Timezone)
		if err != nil {
			return zero, invalid("invalid_cron", err.Error())
		}
		if _, err := schedule.Next(time.Now()); err != nil {
			return zero, invalid("invalid_cron", err.Error())
		}
		config.Cron, config.Timezone = canonicalCron, canonicalTimezone
		if len(config.Targets) == 0 {
			return zero, invalid("no_targets", "a schedule trigger requires at least one target repository")
		}
		targets := make([]InvocationTarget, 0, len(config.Targets))
		seen := make(map[string]bool, len(config.Targets))
		for _, target := range config.Targets {
			identity, err := protocol.NormalizeRepositoryIdentity(target.Repository)
			if err != nil {
				return zero, invalid("invalid_target",
					"target repository "+quote(target.Repository)+": "+err.Error())
			}
			if seen[identity] {
				return zero, invalid("duplicate_target",
					"repository "+quote(identity)+" is listed twice; one run has one job per repository")
			}
			seen[identity] = true
			// Refs are stored, not resolved: pinning belongs to admission
			// (KTD9), so a schedule pins fresh on every firing.
			targets = append(targets, InvocationTarget{
				Repository: identity, Ref: strings.TrimSpace(target.Ref),
			})
		}
		config.Targets = targets
		config.Repository, config.State, config.RequiredLabels = "", "", nil
		config.BaseBranches, config.IncludeDrafts, config.PollIntervalSeconds = nil, false, 0
		return config, nil
	}
	identity, err := protocol.NormalizeRepositoryIdentity(config.Repository)
	if err != nil {
		return zero, invalid("invalid_target",
			"trigger repository "+quote(config.Repository)+": "+err.Error())
	}
	if !strings.HasPrefix(identity, "github.com/") {
		return zero, invalid("invalid_target",
			"a GitHub trigger requires a github.com repository, not "+quote(identity))
	}
	config.Repository = identity
	config.State = strings.ToLower(strings.TrimSpace(config.State))
	if config.State == "" {
		config.State = "open"
	}
	if !validTriggerState(kind, config.State) {
		return zero, invalid("invalid_trigger_state",
			"state "+quote(config.State)+" is not valid for a "+kind+" trigger")
	}
	labels, err := normalizeTriggerLabels(config.RequiredLabels)
	if err != nil {
		return zero, err
	}
	config.RequiredLabels = labels
	if kind == TriggerGitHubPullRequest {
		branches := make([]string, 0, len(config.BaseBranches))
		for _, branch := range config.BaseBranches {
			branch = strings.TrimSpace(branch)
			if branch == "" || len(branch) > 255 {
				return zero, invalid("invalid_base_branch",
					"every base branch must be nonblank and at most 255 bytes")
			}
			branches = append(branches, branch)
		}
		config.BaseBranches = branches
	} else {
		config.BaseBranches, config.IncludeDrafts = nil, false
	}
	interval := time.Duration(config.PollIntervalSeconds) * time.Second
	if interval == 0 {
		interval = protocol.DefaultTriggerPollInterval
	}
	if interval < protocol.MinTriggerPollInterval {
		return zero, invalid("invalid_poll_interval", fmt.Sprintf(
			"poll_interval_seconds must be at least %d", int(protocol.MinTriggerPollInterval.Seconds())))
	}
	config.PollIntervalSeconds = int(interval.Seconds())
	config.Cron, config.Timezone, config.Targets = "", "", nil
	return config, nil
}

func validTriggerState(kind, state string) bool {
	switch state {
	case "open", "closed", "all":
		return true
	case "merged":
		return kind == TriggerGitHubPullRequest
	}
	return false
}

func normalizeTriggerLabels(labels []string) ([]string, error) {
	if len(labels) > 20 {
		return nil, invalid("invalid_required_labels", "a trigger may require at most 20 labels")
	}
	normalized := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" || len(label) > 200 {
			return nil, invalid("invalid_required_labels",
				"every required label must be nonblank and at most 200 bytes")
		}
		normalized = append(normalized, label)
	}
	return normalized, nil
}

func marshalTriggerConfig(config TriggerConfig) (string, error) {
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", unavailable(err)
	}
	if len(encoded) > protocol.MaxTriggerConfigBytes {
		return "", invalid("invalid_trigger_config", "trigger configuration exceeds its storage limit")
	}
	return string(encoded), nil
}

// ---- schedule admission (R13) ----------------------------------------------

// admitDueSchedules admits every enabled schedule whose single stored instant
// has arrived.
func (s *Store) admitDueSchedules(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM triggers
		WHERE kind = 'schedule' AND enabled = 1 AND next_due_at IS NOT NULL AND next_due_at <= ?
		ORDER BY next_due_at, id
	`, s.now().UnixMilli())
	if err != nil {
		return unavailable(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return unavailable(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return unavailable(err)
	}
	var failures error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := s.admitDueSchedule(ctx, id); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// admitDueSchedule admits one schedule's due instant in one transaction:
// either an occurrence exists for it and the cursor advanced, or neither
// happened. The cursor always advances to the next FUTURE match, which is the
// mechanical form of R13's "never catch up" — the instants between the stored
// one and now are stepped over, not queued.
func (s *Store) admitDueSchedule(ctx context.Context, triggerID string) error {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable(err)
	}
	defer tx.Rollback()
	trigger, err := loadTriggerTx(ctx, tx, triggerID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if !trigger.Enabled || trigger.NextDueAt == nil || trigger.NextDueAt.After(now) {
		// Another tick took it, or the operator disabled it between our read
		// of the due list and this transaction.
		return nil
	}
	schedule, _, _, err := parseCronSchedule(trigger.Config.Cron, trigger.Config.Timezone)
	if err != nil {
		// A stored cron that no longer parses can only have arrived through a
		// hand-edited database; the trigger stops rather than guessing.
		return s.parkTriggerTx(ctx, tx, triggerID, "stored_cron_invalid", err.Error(), now)
	}
	nextDue, err := schedule.Next(now)
	if err != nil {
		return s.parkTriggerTx(ctx, tx, triggerID, "stored_cron_invalid", err.Error(), now)
	}
	due := *trigger.NextDueAt
	source := occurrenceSource{
		Kind:        TriggerSchedule,
		ScheduledAt: due.Format(time.RFC3339),
		Cron:        trigger.Config.Cron,
		Timezone:    trigger.Config.Timezone,
	}
	requestKey := "trigger:" + triggerID + ":scheduled:" + due.Format(time.RFC3339)

	state, diagnostic := OccurrencePending, ""
	if reason, overlapping, err := priorFiringStillActive(ctx, tx, triggerID); err != nil {
		return err
	} else if overlapping {
		// R13's overlap rule: the firing is recorded so the history is honest,
		// but it never becomes a run, and the counter says how often this has
		// happened — a schedule quietly outrunning its own work is exactly the
		// condition an operator needs to see.
		state, diagnostic = OccurrenceSkipped, "overlap: "+reason
	} else if !now.Before(due.Add(2 * time.Minute)) {
		// Materially late means the server was not running when this instant
		// passed. It is still admitted — that is the ONE overdue instant R13
		// allows — and the diagnostic says so, because a run appearing hours
		// after its scheduled time is otherwise a mystery.
		diagnostic = "admitted after downtime; later missed instants were not caught up"
	}
	created, err := insertOccurrenceTx(ctx, tx, triggerID, requestKey, state, source, &due, diagnostic, now)
	if err != nil {
		return err
	}
	skipped := 0
	if state == OccurrenceSkipped || !created {
		skipped = 1
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE triggers SET next_due_at = ?, last_checked_at = ?, skipped_count = skipped_count + ?,
		    diagnostic_code = '', diagnostic = '', updated_at = ?
		WHERE id = ? AND next_due_at = ?
	`, nextDue.UnixMilli(), now.UnixMilli(), skipped, now.UnixMilli(),
		triggerID, due.UnixMilli()); err != nil {
		return unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return unavailable(err)
	}
	return nil
}

// priorFiringStillActive reports whether this trigger's previous firing is
// still in flight — either an occurrence that has not dispatched yet, or a
// dispatched occurrence whose run is still `active`. Run aggregation keeps
// that state live, including returning to `active` when a failed job is
// retried, so "still working on the last one" needs no separate bookkeeping.
func priorFiringStillActive(ctx context.Context, tx *sql.Tx, triggerID string) (string, bool, error) {
	var undispatched int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM occurrences
		WHERE trigger_id = ? AND state IN ('pending', 'dispatching')
	`, triggerID).Scan(&undispatched); err != nil {
		return "", false, unavailable(err)
	}
	if undispatched > 0 {
		return "a previous firing has not dispatched yet", true, nil
	}
	var runID, runState string
	err := tx.QueryRowContext(ctx, `
		SELECT run.id, run.state
		FROM occurrences occurrence JOIN runs run ON run.id = occurrence.run_id
		WHERE occurrence.trigger_id = ? AND occurrence.run_id IS NOT NULL
		ORDER BY occurrence.created_at DESC, occurrence.id DESC LIMIT 1
	`, triggerID).Scan(&runID, &runState)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, unavailable(err)
	}
	if runState != protocol.RunActive {
		return "", false, nil
	}
	return "run " + runID + " from the previous firing is still active", true, nil
}

// parkTriggerTx stops a trigger from firing and records why. It clears the
// cursors rather than retrying: the fault is in stored configuration, so
// another tick would only repeat it.
func (s *Store) parkTriggerTx(
	ctx context.Context, tx *sql.Tx, triggerID, code, message string, now time.Time,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE triggers SET next_due_at = NULL, next_poll_at = NULL, last_checked_at = ?,
		    diagnostic_code = ?, diagnostic = ?, updated_at = ?
		WHERE id = ?
	`, now.UnixMilli(), code, boundedDiagnostic(message), now.UnixMilli(), triggerID); err != nil {
		return unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return unavailable(err)
	}
	return nil
}

// ---- occurrences: commit, then dispatch ------------------------------------

// insertOccurrenceTx commits one occurrence under its UNIQUE request key. A
// key collision is not an error: it is the idempotency contract working — some
// earlier firing, poll, or process already recorded this exact event — so it
// reports "not created" and the caller counts it as a skip.
func insertOccurrenceTx(
	ctx context.Context, tx *sql.Tx, triggerID, requestKey, state string,
	source occurrenceSource, scheduledAt *time.Time, diagnostic string, now time.Time,
) (bool, error) {
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		return false, unavailable(err)
	}
	id, err := newID()
	if err != nil {
		return false, unavailable(err)
	}
	var scheduled any
	if scheduledAt != nil {
		scheduled = scheduledAt.UnixMilli()
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO occurrences(id, trigger_id, request_key, state, source, scheduled_at,
			diagnostic, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(request_key) DO NOTHING
	`, id, triggerID, requestKey, state, string(sourceJSON), scheduled,
		boundedDiagnostic(diagnostic), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return false, unavailable(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, unavailable(err)
	}
	return changed == 1, nil
}

// RecoverOccurrences returns every reserved-but-undispatched occurrence to
// `pending` so startup can re-drive it. It is safe precisely because a run
// and its dispatched mark commit together: a `dispatching` row is proof that
// no run was created, never a row whose run might already exist.
func (s *Store) RecoverOccurrences(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE occurrences
		SET state = 'pending', diagnostic = 'recovered: dispatch was interrupted', updated_at = ?
		WHERE state = 'dispatching'
	`, s.now().UnixMilli())
	if err != nil {
		return 0, unavailable(err)
	}
	recovered, err := result.RowsAffected()
	if err != nil {
		return 0, unavailable(err)
	}
	return int(recovered), nil
}

// dispatchPendingOccurrences drives every pending occurrence to a run.
func (s *Store) dispatchPendingOccurrences(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM occurrences WHERE state = 'pending' ORDER BY created_at, id
	`)
	if err != nil {
		return unavailable(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return unavailable(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return unavailable(err)
	}
	var failures error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := s.DispatchOccurrence(ctx, id); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// DispatchOccurrence turns one committed occurrence into exactly one run.
//
// Three steps, in this order and no other:
//
//  1. Reserve the row (`pending` -> `dispatching`). Losing this race means
//     someone else owns the dispatch; there is nothing to do and nothing to
//     report.
//  2. Resolve the invocation OUTSIDE any transaction — this is where base
//     SHAs get pinned (KTD9), which touches the network and must never hold
//     a write lock.
//  3. Create the run and mark the occurrence dispatched in ONE transaction.
//     This is the step that makes exactly-once true: there is no ordering of
//     crashes that yields a run whose occurrence still looks undispatched.
//
// A dispatch that cannot resolve — an unreachable ref, an unknown
// repository, a definition deleted since the trigger was saved — fails the
// occurrence with the diagnostic code the store produced, and does not retry:
// a poll that re-observes the same event collides with the same request key,
// so silent retrying would be indistinguishable from a hung trigger.
func (s *Store) DispatchOccurrence(ctx context.Context, occurrenceID string) error {
	now := s.now()
	reserved, err := s.db.ExecContext(ctx, `
		UPDATE occurrences SET state = 'dispatching', updated_at = ?
		WHERE id = ? AND state = 'pending'
	`, now.UnixMilli(), occurrenceID)
	if err != nil {
		return unavailable(err)
	}
	if changed, err := reserved.RowsAffected(); err != nil {
		return unavailable(err)
	} else if changed != 1 {
		return nil
	}
	occurrence, trigger, err := s.occurrenceWithTrigger(ctx, occurrenceID)
	if err != nil {
		return err
	}
	invocation, err := buildInvocation(trigger, occurrence)
	if err != nil {
		return s.failOccurrence(ctx, occurrenceID, err)
	}
	prepared, err := s.prepareInvocation(ctx, invocation)
	if err != nil {
		return s.failOccurrence(ctx, occurrenceID, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable(err)
	}
	defer tx.Rollback()
	runID, _, err := admitRunTx(ctx, tx, prepared, now.UnixMilli())
	if err != nil {
		tx.Rollback()
		return s.failOccurrence(ctx, occurrenceID, err)
	}
	// The diagnostic is left as it stands: it explains how this occurrence
	// came to be — admitted after downtime, recovered from an interrupted
	// dispatch — and that history outlives the dispatch itself.
	result, err := tx.ExecContext(ctx, `
		UPDATE occurrences SET state = 'dispatched', run_id = ?, updated_at = ?
		WHERE id = ? AND state = 'dispatching'
	`, runID, now.UnixMilli(), occurrenceID)
	if err != nil {
		return unavailable(err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return unavailable(err)
	} else if changed != 1 {
		// The reservation was taken from us mid-dispatch. Rolling back takes
		// the run with it, which is the entire reason the two share a
		// transaction.
		return conflict("occurrence_reservation_lost",
			"the occurrence left `dispatching` during its dispatch")
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE triggers SET admitted_count = admitted_count + 1, updated_at = ? WHERE id = ?
	`, now.UnixMilli(), occurrence.TriggerID); err != nil {
		return unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return unavailable(err)
	}
	return nil
}

// failOccurrence records why a dispatch could not produce a run. The store's
// own diagnostic code travels verbatim — `unresolvable_ref`, `invalid_target`,
// `ambiguous_prompt`, `not_found` — because it is already the actionable name
// of the fault.
func (s *Store) failOccurrence(ctx context.Context, occurrenceID string, cause error) error {
	code, message := "dispatch_failed", cause.Error()
	var service *ServiceError
	if errors.As(cause, &service) {
		code, message = service.Code, service.Message
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE occurrences SET state = 'failed', diagnostic = ?, updated_at = ?
		WHERE id = ? AND state IN ('pending', 'dispatching')
	`, boundedDiagnostic(code+": "+message), s.now().UnixMilli(), occurrenceID); err != nil {
		return unavailable(err)
	}
	return nil
}

// buildInvocation composes the run invocation from the trigger's trusted
// instructions and the occurrence's frozen observation. The observation is
// ALWAYS untrusted context, never instructions and never a raw parameter: an
// issue body is written by whoever can open an issue (KTD11), and this is the
// seam where that distinction is made once, for every trigger kind.
func buildInvocation(trigger Trigger, occurrence Occurrence) (RunInvocation, error) {
	var source occurrenceSource
	if err := json.Unmarshal(occurrence.Source, &source); err != nil {
		return RunInvocation{}, invalid("invalid_occurrence_source",
			"the occurrence's frozen source is unreadable: "+err.Error())
	}
	invocation := RunInvocation{
		DefinitionID: trigger.DefinitionID,
		Instructions: trigger.Config.Instructions,
		Parameters:   trigger.Config.Parameters,
	}
	switch trigger.Kind {
	case TriggerSchedule:
		invocation.Targets = trigger.Config.Targets
		invocation.Context = []protocol.UntrustedSection{{
			Label: "schedule",
			Body: "This run was admitted by the schedule " + trigger.Config.Cron +
				" (" + trigger.Config.Timezone + ") for the instant " + source.ScheduledAt + ".",
		}}
	case TriggerGitHubIssue, TriggerGitHubPullRequest:
		target := InvocationTarget{Repository: source.Repository}
		if isCommitSHA(source.HeadSHA) {
			// The poll already knows the pull request's head, so admission
			// pins it directly instead of asking the network what the
			// repository's default branch points at — which is not the commit
			// this occurrence is about.
			target.BaseSHA = source.HeadSHA
		}
		invocation.Targets = []InvocationTarget{target}
		invocation.Context = []protocol.UntrustedSection{{
			Label: source.contextLabel(),
			Body:  source.contextBody(),
		}}
	default:
		return RunInvocation{}, invalid("invalid_trigger_kind",
			"trigger kind "+quote(trigger.Kind)+" cannot be dispatched")
	}
	return invocation, nil
}

func (source occurrenceSource) contextLabel() string {
	if source.Kind == TriggerGitHubPullRequest {
		return "pull-request-" + strconv.Itoa(source.Number)
	}
	return "issue-" + strconv.Itoa(source.Number)
}

func (source occurrenceSource) contextBody() string {
	var builder strings.Builder
	builder.WriteString("repository: " + source.Repository + "\n")
	builder.WriteString("number: " + strconv.Itoa(source.Number) + "\n")
	builder.WriteString("title: " + source.Title + "\n")
	builder.WriteString("url: " + source.URL + "\n")
	if len(source.Labels) > 0 {
		builder.WriteString("labels: " + strings.Join(source.Labels, ", ") + "\n")
	}
	if source.BaseBranch != "" {
		builder.WriteString("base branch: " + source.BaseBranch + "\n")
	}
	if source.HeadSHA != "" {
		builder.WriteString("head commit: " + source.HeadSHA + "\n")
	}
	builder.WriteString("\n")
	builder.WriteString(source.Body)
	return builder.String()
}

func boundedDiagnostic(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= protocol.MaxTriggerDiagnosticBytes {
		return value
	}
	return value[:protocol.MaxTriggerDiagnosticBytes]
}

// ---- the admission loop ----------------------------------------------------

// AdmissionRunner drives unattended admission: due schedules, due GitHub
// polls, and pending dispatches, on one tick. `jig serve` owns it (U9); it is
// deliberately a plain struct with an explicit Tick so tests drive admission
// deterministically instead of waiting on a clock.
type AdmissionRunner struct {
	store   *Store
	gateway GitHubGateway
	logger  *slog.Logger
}

// NewAdmissionRunner builds the runner. A nil gateway disables GitHub polling
// while leaving schedules working — a server with no `gh` on PATH is still a
// working scheduler.
func NewAdmissionRunner(store *Store, gateway GitHubGateway, logger *slog.Logger) *AdmissionRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &AdmissionRunner{store: store, gateway: gateway, logger: logger}
}

// Recover re-drives occurrences interrupted mid-dispatch. It must run before
// the first Tick — that is what closes the crash-between-commit-and-dispatch
// window (R13).
func (a *AdmissionRunner) Recover(ctx context.Context) error {
	recovered, err := a.store.RecoverOccurrences(ctx)
	if err != nil {
		return err
	}
	if recovered > 0 {
		a.logger.Info("admission_recovered", "occurrences", recovered)
	}
	return a.store.dispatchPendingOccurrences(ctx)
}

// Tick performs one admission pass. Failures in one stage never stop the
// others: a GitHub outage must not stop schedules, and a failed dispatch must
// not stop the next poll.
func (a *AdmissionRunner) Tick(ctx context.Context) error {
	var failures error
	if err := a.store.admitDueSchedules(ctx); err != nil {
		failures = errors.Join(failures, err)
	}
	if a.gateway != nil {
		if err := a.store.pollDueGitHubTriggers(ctx, a.gateway); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	if err := a.store.dispatchPendingOccurrences(ctx); err != nil {
		failures = errors.Join(failures, err)
	}
	return failures
}

// Run ticks until ctx ends.
func (a *AdmissionRunner) Run(ctx context.Context) {
	ticker := time.NewTicker(protocol.TriggerTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.Tick(ctx); err != nil && ctx.Err() == nil {
				a.logger.Error("admission_tick_failed", "error", err)
			}
		}
	}
}

// ---- reads -----------------------------------------------------------------

const triggerColumns = `id, definition_id, name, kind, enabled, config, next_due_at, next_poll_at,
	last_checked_at, admitted_count, skipped_count, diagnostic_code, diagnostic, created_at, updated_at`

func scanTrigger(row rowScanner) (Trigger, error) {
	var value Trigger
	var enabled int
	var configJSON string
	var nextDue, nextPoll, lastChecked sql.NullInt64
	var createdAt, updatedAt int64
	if err := row.Scan(&value.ID, &value.DefinitionID, &value.Name, &value.Kind, &enabled,
		&configJSON, &nextDue, &nextPoll, &lastChecked, &value.AdmittedCount, &value.SkippedCount,
		&value.DiagnosticCode, &value.Diagnostic, &createdAt, &updatedAt); err != nil {
		return value, err
	}
	if err := json.Unmarshal([]byte(configJSON), &value.Config); err != nil {
		return value, err
	}
	value.Enabled = enabled != 0
	value.NextDueAt = timePointer(nextDue)
	value.NextPollAt = timePointer(nextPoll)
	value.LastCheckedAt = timePointer(lastChecked)
	value.CreatedAt = fromMillis(createdAt)
	value.UpdatedAt = fromMillis(updatedAt)
	return value, nil
}

func loadTriggerTx(ctx context.Context, tx *sql.Tx, triggerID string) (Trigger, error) {
	value, err := scanTrigger(tx.QueryRowContext(ctx,
		`SELECT `+triggerColumns+` FROM triggers WHERE id = ?`, strings.TrimSpace(triggerID)))
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, unavailable(err)
	}
	return value, nil
}

// Trigger reads one trigger.
func (s *Store) Trigger(ctx context.Context, triggerID string) (Trigger, error) {
	value, err := scanTrigger(s.db.QueryRowContext(ctx,
		`SELECT `+triggerColumns+` FROM triggers WHERE id = ?`, strings.TrimSpace(triggerID)))
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, unavailable(err)
	}
	return value, nil
}

// Triggers lists every trigger by name.
func (s *Store) Triggers(ctx context.Context) ([]Trigger, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+triggerColumns+` FROM triggers ORDER BY name`)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	values := []Trigger{}
	for rows.Next() {
		value, err := scanTrigger(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return values, nil
}

const occurrenceColumns = `id, trigger_id, request_key, state, run_id, source, scheduled_at,
	diagnostic, created_at, updated_at`

func scanOccurrence(row rowScanner) (Occurrence, error) {
	var value Occurrence
	var runID sql.NullString
	var source string
	var scheduledAt sql.NullInt64
	var createdAt, updatedAt int64
	if err := row.Scan(&value.ID, &value.TriggerID, &value.RequestKey, &value.State, &runID,
		&source, &scheduledAt, &value.Diagnostic, &createdAt, &updatedAt); err != nil {
		return value, err
	}
	value.RunID = runID.String
	value.Source = json.RawMessage(source)
	value.ScheduledAt = timePointer(scheduledAt)
	value.CreatedAt = fromMillis(createdAt)
	value.UpdatedAt = fromMillis(updatedAt)
	return value, nil
}

// Occurrence reads one occurrence.
func (s *Store) Occurrence(ctx context.Context, occurrenceID string) (Occurrence, error) {
	value, err := scanOccurrence(s.db.QueryRowContext(ctx,
		`SELECT `+occurrenceColumns+` FROM occurrences WHERE id = ?`, occurrenceID))
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, unavailable(err)
	}
	return value, nil
}

func (s *Store) occurrenceWithTrigger(ctx context.Context, occurrenceID string) (Occurrence, Trigger, error) {
	occurrence, err := s.Occurrence(ctx, occurrenceID)
	if err != nil {
		return Occurrence{}, Trigger{}, err
	}
	trigger, err := s.Trigger(ctx, occurrence.TriggerID)
	if err != nil {
		return Occurrence{}, Trigger{}, err
	}
	return occurrence, trigger, nil
}

// TriggerOccurrences lists one trigger's occurrences, newest first.
func (s *Store) TriggerOccurrences(ctx context.Context, triggerID string) ([]Occurrence, error) {
	if _, err := s.Trigger(ctx, triggerID); err != nil {
		return nil, err
	}
	return s.occurrences(ctx, `WHERE trigger_id = ?`, triggerID)
}

// Occurrences lists every occurrence, newest first.
func (s *Store) Occurrences(ctx context.Context) ([]Occurrence, error) {
	return s.occurrences(ctx, ``)
}

func (s *Store) occurrences(ctx context.Context, where string, arguments ...any) ([]Occurrence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+occurrenceColumns+` FROM occurrences `+where+` ORDER BY created_at DESC, id`,
		arguments...)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	values := []Occurrence{}
	for rows.Next() {
		value, err := scanOccurrence(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return values, nil
}

// ---- HTTP surface ----------------------------------------------------------

// registerTriggerRoutes attaches the admission surface, following the publish
// ledger's pattern: one line in http.go, every handler beside the store
// methods it calls. Every state-changing route takes prepareMutation, so the
// Origin fence covers trigger authoring exactly as it covers invocation (R20).
func (a *API) registerTriggerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/triggers", a.createTrigger)
	mux.HandleFunc("GET /api/triggers", a.listTriggers)
	mux.HandleFunc("GET /api/triggers/{trigger_id}", a.getTrigger)
	mux.HandleFunc("PUT /api/triggers/{trigger_id}", a.updateTrigger)
	mux.HandleFunc("DELETE /api/triggers/{trigger_id}", a.deleteTrigger)
	mux.HandleFunc("POST /api/triggers/{trigger_id}/enable", a.enableTrigger)
	mux.HandleFunc("POST /api/triggers/{trigger_id}/disable", a.disableTrigger)
	mux.HandleFunc("GET /api/triggers/{trigger_id}/occurrences", a.listTriggerOccurrences)
	mux.HandleFunc("GET /api/occurrences", a.listOccurrences)
}

func (a *API) createTrigger(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input TriggerInput
	if !decodeJSON(w, r, &input) {
		return
	}
	trigger, err := a.store.CreateTrigger(r.Context(), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, trigger)
}

func (a *API) updateTrigger(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input TriggerInput
	if !decodeJSON(w, r, &input) {
		return
	}
	trigger, err := a.store.UpdateTrigger(r.Context(), r.PathValue("trigger_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, trigger)
}

func (a *API) deleteTrigger(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	if err := a.store.DeleteTrigger(r.Context(), r.PathValue("trigger_id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// enableTrigger and disableTrigger are bodyless POSTs: enabling takes nothing
// but the trigger, and making it a PUT of {"enabled": true} would invite a
// caller to send the rest of the trigger with it.
func (a *API) enableTrigger(w http.ResponseWriter, r *http.Request) {
	a.setTriggerEnabled(w, r, true)
}

func (a *API) disableTrigger(w http.ResponseWriter, r *http.Request) {
	a.setTriggerEnabled(w, r, false)
}

func (a *API) setTriggerEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	if !a.prepareMutation(w, r) {
		return
	}
	trigger, err := a.store.SetTriggerEnabled(r.Context(), r.PathValue("trigger_id"), enabled)
	if err != nil {
		writeError(w, err)
		return
	}
	a.logger.Info("state_change", "resource_type", "trigger", "resource_id", trigger.ID,
		"new_state", triggerStateName(trigger.Enabled))
	writeJSON(w, http.StatusOK, trigger)
}

func triggerStateName(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func (a *API) getTrigger(w http.ResponseWriter, r *http.Request) {
	trigger, err := a.store.Trigger(r.Context(), r.PathValue("trigger_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, trigger)
}

func (a *API) listTriggers(w http.ResponseWriter, r *http.Request) {
	triggers, err := a.store.Triggers(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, triggers)
}

func (a *API) listTriggerOccurrences(w http.ResponseWriter, r *http.Request) {
	occurrences, err := a.store.TriggerOccurrences(r.Context(), r.PathValue("trigger_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, occurrences)
}

func (a *API) listOccurrences(w http.ResponseWriter, r *http.Request) {
	occurrences, err := a.store.Occurrences(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, occurrences)
}

// sortedLabels keeps a match's labels in a stable order so an occurrence's
// frozen source is byte-comparable across polls.
func sortedLabels(labels []string) []string {
	sorted := append([]string(nil), labels...)
	sort.Strings(sorted)
	return sorted
}
