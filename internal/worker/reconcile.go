// reconcile.go — startup reconciliation and fail-closed disposal (U3, R16).
//
// At worker start, disk worktrees, manifests, and the control-plane ledger
// are compared in both directions: a manifest whose worktree survives is
// retained (never deleted on restart), a manifest whose worktree vanished
// marks its ledger row lost, and an on-disk worktree with no manifest is an
// orphan — reported to the operator, touched by nobody.
//
// Reconciliation also stops the agent process groups a crashed worker left
// running (U4's write side records them). Stopping is identity-gated: pids
// recycle, so a recorded group is signalled only when the OS still shows its
// leader leading that exact group and started inside the window the manifest
// records. Anything less certain is left alone and reported — killing a
// stranger's process is worse than leaking one of ours.
//
// Disposal after a completed attempt fails closed: delete only what is
// provably worthless (clean at base) or provably published (remote-ref
// proof); everything else is retained with a reason and surfaced through the
// ledger, where the operator release action (confirmation-gated,
// control-plane-side) is the only other road to deletion.
package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

const (
	// processTerminationGrace is how long a recorded process group gets after
	// TERM before it is KILLed.
	processTerminationGrace = 3 * time.Second

	// processTerminationPoll is how often the grace window is re-checked.
	processTerminationPoll = 25 * time.Millisecond

	// processIdentitySlack absorbs the one-second resolution of the OS start
	// time and the gap between a process starting and its manifest write.
	processIdentitySlack = 30 * time.Second

	// processStartLayout parses `ps -o lstart=` on both supported platforms.
	processStartLayout = "Mon Jan 2 15:04:05 2006"

	// minimumSignallableProcessGroup is the smallest recorded group id that
	// could ever name an attempt's own agent group. The values below it are
	// not merely useless, they are catastrophic: `kill(-1, ...)` signals every
	// process the caller may signal — the operator's entire session — and
	// `kill(0, ...)` signals jig's own group. A zeroed, corrupted, or
	// hand-edited manifest field must never reach that syscall.
	minimumSignallableProcessGroup = 2
)

// signallableProcessGroup reports whether a recorded group id could possibly
// name an attempt's own process group. Everything at or below 1 is
// unverifiable by construction: no identity check can make -1, 0, or 1 ours.
//
// The value must also survive the int64 -> int conversion every signal path
// performs, because on a 32-bit build that conversion truncates — and a
// truncated id is not merely the wrong group. 4294967296 truncates to 0,
// 4294967297 to 1, 4294967298 to 2: the machine-wide `kill(-1, ...)` and
// jig's-own-group cases this predicate exists to refuse, reintroduced
// silently *after* the range check has already passed. An id this platform
// cannot represent names no local process group, so it is refused rather
// than reinterpreted.
func signallableProcessGroup(groupID int64) bool {
	return groupID >= minimumSignallableProcessGroup && int64(int(groupID)) == groupID
}

// ReconcileReport is what one startup reconciliation observed and did.
type ReconcileReport struct {
	// Retained worktrees survived the restart and were (re)reported to the
	// ledger.
	Retained []protocol.RetainedWorktree
	// MissingAttemptIDs had a manifest or ledger row but no disk; their
	// ledger rows were marked lost.
	MissingAttemptIDs []string
	// OrphanPaths exist on disk with no manifest. Reported, never deleted.
	OrphanPaths []string
	// StoppedProcessGroups are the agent process groups this reconciliation
	// verified as ours and stopped.
	StoppedProcessGroups []int64
	// ReleasedAttemptIDs had an operator-released ledger row; their worktrees
	// were removed under operator confirmation.
	ReleasedAttemptIDs []string
	// Ledger is the control plane's view after the report was applied.
	Ledger []protocol.WorktreeLedgerEntry
}

// Reconcile compares disk ↔ manifests ↔ control-plane ledger in both
// directions and reports the result. The worker must be registered first —
// the ledger is scoped to a known worker.
func (w *Worker) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var report ReconcileReport
	disposals, err := w.manifests.loadDisposals()
	if err != nil {
		return report, fmt.Errorf("load disposal journal: %w", err)
	}
	disposed := make(map[string]bool, len(disposals))
	for _, attemptID := range disposals {
		disposed[attemptID] = true
	}
	manifests, loadErr := w.manifests.loadAll()
	if loadErr != nil {
		// Unreadable manifests block nothing else, but they are surfaced:
		// whatever they covered stays on disk untouched (fail closed).
		w.logger.Warn("manifest_load_incomplete", "error", loadErr)
	}
	// Processes first: an agent still writing into a worktree must be stopped
	// before anything reasons about that worktree's contents.
	report.StoppedProcessGroups = w.stopRecordedProcessGroups(ctx, manifests)
	ledger, err := w.client.Worktrees(ctx)
	if err != nil {
		return report, fmt.Errorf("read worktree ledger: %w", err)
	}
	ledgerByID := make(map[string]protocol.WorktreeLedgerEntry, len(ledger))
	for _, entry := range ledger {
		ledgerByID[entry.AttemptID] = entry
	}

	manifestPaths := make(map[string]bool, len(manifests))
	seen := make(map[string]bool, len(manifests))
	for _, manifest := range manifests {
		manifestPaths[manifest.WorktreePath] = true
		if disposed[manifest.AttemptID] {
			continue
		}
		seen[manifest.AttemptID] = true
		onDisk, diskErr := worktreeDiskState(manifest.WorktreePath)
		if diskErr != nil {
			// Integrity doubt: retain and surface, never resolve by deleting.
			w.logger.Warn("worktree_integrity_doubt",
				"attempt_id", manifest.AttemptID, "error", diskErr)
			report.Retained = append(report.Retained, w.retainManifest(manifest,
				"worktree integrity doubt: "+diskErr.Error()))
			continue
		}
		if !onDisk {
			// Manifest without disk: record it missing and, when the ledger
			// still holds it retained, mark it lost server-side.
			if _, err := w.manifests.update(manifest.AttemptID, func(value *attemptManifest) error {
				value.Lifecycle = manifestMissing
				value.RetentionReason = "previously created worktree is absent"
				return nil
			}); err != nil {
				return report, err
			}
			if err := w.manifests.addDisposal(manifest.AttemptID); err != nil {
				return report, err
			}
			w.forgetRetained(manifest.AttemptID)
			if entry, exists := ledgerByID[manifest.AttemptID]; exists && entry.State == protocol.WorktreeRetained {
				report.MissingAttemptIDs = append(report.MissingAttemptIDs, manifest.AttemptID)
			}
			continue
		}
		if entry, exists := ledgerByID[manifest.AttemptID]; exists && entry.State == protocol.WorktreeReleased {
			// The operator inspected and released this worktree: the release
			// action is the one deletion path that needs no publish proof.
			if err := w.removeReleasedWorktree(ctx, manifest); err != nil {
				w.logger.Warn("released_worktree_removal_failed",
					"attempt_id", manifest.AttemptID, "error", err)
				report.Retained = append(report.Retained, w.retainManifest(manifest,
					"operator release could not be applied: "+err.Error()))
				continue
			}
			report.ReleasedAttemptIDs = append(report.ReleasedAttemptIDs, manifest.AttemptID)
			continue
		}
		reason := manifest.RetentionReason
		if reason == "" {
			reason = "worktree retained after worker restart"
		}
		report.Retained = append(report.Retained, w.retainManifest(manifest, reason))
	}

	// Ledger rows with no manifest coverage: if their recorded path is gone
	// too, they are lost; if something is still there, it stays untouched.
	for _, entry := range ledger {
		if entry.State != protocol.WorktreeRetained || seen[entry.AttemptID] || disposed[entry.AttemptID] {
			continue
		}
		onDisk, diskErr := worktreeDiskState(entry.Path)
		if diskErr == nil && !onDisk {
			report.MissingAttemptIDs = append(report.MissingAttemptIDs, entry.AttemptID)
		}
	}

	// Orphans: on-disk worktrees no manifest owns. Report, never delete.
	orphans, err := w.scanOrphanWorktrees(manifestPaths)
	if err != nil {
		return report, err
	}
	report.OrphanPaths = orphans
	for _, orphan := range orphans {
		w.logger.Warn("orphan_worktree_found", "path", orphan)
	}

	result, err := w.client.ReconcileWorktrees(ctx, protocol.WorktreeReconciliationReport{
		Retained:          report.Retained,
		MissingAttemptIDs: report.MissingAttemptIDs,
		OrphanPaths:       report.OrphanPaths,
	})
	if err != nil {
		return report, fmt.Errorf("report reconciliation: %w", err)
	}
	report.Ledger = result.Ledger
	return report, nil
}

// ---- process-group reconciliation (U4's read side) -------------------------

// stopRecordedProcessGroups stops every agent process group a previous worker
// process recorded as live. Each candidate is identity-checked first: a
// recorded group id is just a number, and by the time we read it the pid may
// belong to someone else's shell — or to nobody at all, if the field was
// zeroed or corrupted. Only a group id that can name a real group AND whose
// leader still leads that exact group AND that started inside the manifest's
// own lifetime is ours to signal. Either way the flag is cleared, so one
// unverifiable manifest cannot make every later reconciliation re-examine it
// forever.
func (w *Worker) stopRecordedProcessGroups(ctx context.Context, manifests []attemptManifest) []int64 {
	var stopped []int64
	for _, manifest := range manifests {
		if !manifest.ProcessActive {
			continue
		}
		groupID := manifest.ProcessGroupID
		ours, reason := processGroupIsOurs(ctx, manifest)
		if ours {
			stopProcessGroup(groupID)
			stopped = append(stopped, groupID)
			w.logger.Info("orphan_process_group_stopped",
				"attempt_id", manifest.AttemptID, "process_group_id", groupID)
		} else {
			// Not provably ours: never signalled. A recycled pid belongs to
			// someone else, and this is the line where that is decided.
			w.logger.Info("orphan_process_group_skipped",
				"attempt_id", manifest.AttemptID, "process_group_id", groupID, "reason", reason)
		}
		if _, err := w.manifests.update(manifest.AttemptID, func(value *attemptManifest) error {
			value.ProcessActive = false
			return nil
		}); err != nil {
			w.logger.Warn("process_group_clear_failed",
				"attempt_id", manifest.AttemptID, "error", err)
		}
	}
	return stopped
}

// processGroupIsOurs answers whether the recorded group's leader is still the
// process this manifest recorded, and says why when it is not.
func processGroupIsOurs(ctx context.Context, manifest attemptManifest) (bool, string) {
	if !signallableProcessGroup(manifest.ProcessGroupID) {
		// -1 signals every process the operator's user may signal, 0 signals
		// jig's own group, 1 is init. None can be an attempt's agent group, so
		// there is nothing here to verify and nothing to signal.
		return false, fmt.Sprintf(
			"recorded process group %d can never name an attempt's own group",
			manifest.ProcessGroupID)
	}
	groupID, started, found, err := inspectProcessGroupLeader(ctx, manifest.ProcessGroupID)
	switch {
	case err != nil:
		return false, "process identity could not be read: " + err.Error()
	case !found:
		return false, "the recorded group leader no longer exists"
	case groupID != manifest.ProcessGroupID:
		return false, fmt.Sprintf("pid %d now leads group %d, not %d",
			manifest.ProcessGroupID, groupID, manifest.ProcessGroupID)
	}
	earliest := manifest.CreatedAt.Add(-processIdentitySlack)
	latest := manifest.UpdatedAt.Add(processIdentitySlack)
	if started.Before(earliest) || started.After(latest) {
		return false, fmt.Sprintf("pid %d started at %s, outside this attempt's lifetime (%s..%s) — recycled",
			manifest.ProcessGroupID, started.UTC().Format(time.RFC3339),
			earliest.UTC().Format(time.RFC3339), latest.UTC().Format(time.RFC3339))
	}
	return true, ""
}

// inspectProcessGroupLeader reads the process group and start time of one pid
// through ps — the portable answer on both supported platforms (macOS and
// Linux). found=false means the pid is gone.
func inspectProcessGroupLeader(ctx context.Context, pid int64) (groupID int64, started time.Time, found bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.GitCommandTimeout)
	defer cancel()
	output, runErr := exec.CommandContext(ctx,
		"ps", "-p", strconv.FormatInt(pid, 10), "-o", "pgid=,lstart=").Output()
	if runErr != nil {
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			// ps exits nonzero when the pid does not exist.
			return 0, time.Time{}, false, nil
		}
		return 0, time.Time{}, false, fmt.Errorf("inspect pid %d: %w", pid, runErr)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 6 {
		return 0, time.Time{}, false, fmt.Errorf("unreadable ps answer for pid %d: %q", pid, output)
	}
	groupID, parseErr := strconv.ParseInt(fields[0], 10, 64)
	if parseErr != nil {
		return 0, time.Time{}, false, fmt.Errorf("unreadable process group for pid %d: %q", pid, output)
	}
	started, parseErr = time.ParseInLocation(processStartLayout, strings.Join(fields[1:6], " "), time.Local)
	if parseErr != nil {
		return 0, time.Time{}, false, fmt.Errorf("unreadable start time for pid %d: %q", pid, output)
	}
	return groupID, started, true, nil
}

// stopProcessGroup TERMs the whole group, waits out the grace window, then
// KILLs whatever remains. ESRCH means already gone, which is success.
//
// The range guard is the last gate before the syscall and duplicates the
// caller's on purpose: this function negates its argument, so a group id of 1
// would become `kill(-1, SIGTERM)` — every process the operator's user may
// signal. That is one bad integer away from destroying the machine's session,
// and it must not depend on any caller remembering to check first. The gate
// covers the narrowing below as well, so pgid is the recorded value and not a
// truncation of it.
func stopProcessGroup(groupID int64) {
	if !signallableProcessGroup(groupID) {
		return
	}
	pgid := int(groupID)
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		return
	}
	deadline := time.Now().Add(processTerminationGrace)
	for time.Now().Before(deadline) {
		time.Sleep(processTerminationPoll)
		if err := syscall.Kill(-pgid, 0); err != nil {
			return
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// retainManifest marks one manifest retained, remembers it for registration
// payloads, and returns the ledger-shaped record.
func (w *Worker) retainManifest(manifest attemptManifest, reason string) protocol.RetainedWorktree {
	reason = boundedText(reason, protocol.MaxRetentionReasonBytes)
	if _, err := w.manifests.update(manifest.AttemptID, func(value *attemptManifest) error {
		value.Lifecycle = manifestRetained
		value.RetentionReason = reason
		return nil
	}); err != nil {
		w.logger.Warn("manifest_retention_persist_failed",
			"attempt_id", manifest.AttemptID, "error", err)
	}
	entry := protocol.RetainedWorktree{
		AttemptID:  manifest.AttemptID,
		Repository: manifest.Repository,
		Path:       manifest.WorktreePath,
		Reason:     reason,
	}
	w.recordRetained(entry)
	return entry
}

// removeReleasedWorktree applies an operator release: force removal, because
// the operator's confirmation replaces the publish proof (R16).
func (w *Worker) removeReleasedWorktree(ctx context.Context, manifest attemptManifest) error {
	if _, err := w.manifests.update(manifest.AttemptID, func(value *attemptManifest) error {
		value.Lifecycle = manifestCleanupStarted
		value.CleanupIntent = cleanupIntentOperator
		return nil
	}); err != nil {
		return err
	}
	if err := w.removeWorktree(ctx, manifest, true); err != nil {
		return err
	}
	if _, err := w.manifests.update(manifest.AttemptID, func(value *attemptManifest) error {
		value.Lifecycle = manifestCleaned
		value.CleanupResult = "removed under operator release confirmation"
		value.RetentionReason = ""
		return nil
	}); err != nil {
		return err
	}
	w.forgetRetained(manifest.AttemptID)
	return w.manifests.addDisposal(manifest.AttemptID)
}

// scanOrphanWorktrees lists directories under the worktree root that no
// manifest (disposed or live) accounts for.
func (w *Worker) scanOrphanWorktrees(manifestPaths map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(w.worktreeRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan worktree root: %w", err)
	}
	var orphans []string
	for _, entry := range entries {
		path := filepath.Join(w.worktreeRoot(), entry.Name())
		if !manifestPaths[path] {
			orphans = append(orphans, path)
		}
	}
	return orphans, nil
}

// disposeAttemptWorktree runs after a completed attempt: delete only with
// proof (clean at base, or remote-ref proof of publish), retain with a
// reason otherwise. Retention is a success, not an error.
func (w *Worker) disposeAttemptWorktree(ctx context.Context, attemptID string) error {
	manifest, err := w.manifests.load(attemptID)
	if err != nil {
		return err
	}
	if eligibleErr := w.cleanupEligible(ctx, manifest); eligibleErr != nil {
		w.retainAndReport(ctx, manifest, eligibleErr.Error())
		return nil
	}
	if _, err := w.manifests.update(attemptID, func(value *attemptManifest) error {
		value.Lifecycle = manifestCleanupStarted
		value.CleanupIntent = cleanupIntentAutomatic
		return nil
	}); err != nil {
		return err
	}
	manifest, err = w.manifests.load(attemptID)
	if err != nil {
		return err
	}
	// Re-verify after declaring intent: the eligibility that was true a
	// moment ago must still be true when the deletion actually happens.
	if eligibleErr := w.cleanupEligible(ctx, manifest); eligibleErr != nil {
		w.retainAndReport(ctx, manifest, eligibleErr.Error())
		return nil
	}
	if err := w.removeWorktree(ctx, manifest, false); err != nil {
		w.retainAndReport(ctx, manifest, "worktree removal failed: "+err.Error())
		return nil
	}
	if _, err := w.manifests.update(attemptID, func(value *attemptManifest) error {
		value.Lifecycle = manifestCleaned
		value.CleanupResult = "automatic cleanup with publish proof or clean base"
		value.RetentionReason = ""
		return nil
	}); err != nil {
		return err
	}
	w.forgetRetained(attemptID)
	return w.manifests.addDisposal(attemptID)
}

// retainAfterAttempt retains an attempt's worktree by ID when the attempt's
// outcome could not be recorded — uncertainty retains (R16). A manifest that
// never produced a worktree disposes instead.
func (w *Worker) retainAfterAttempt(ctx context.Context, attemptID, reason string) {
	manifest, err := w.manifests.load(attemptID)
	if err != nil {
		w.logger.Warn("retention_manifest_unavailable", "attempt_id", attemptID, "error", err)
		return
	}
	if manifest.Lifecycle == manifestNotCreated {
		return
	}
	w.retainAndReport(ctx, manifest, reason)
}

// retainAndReport persists retention locally and pushes it into the ledger
// immediately, so the operator sees the retained worktree and its release
// action without waiting for the next registration tick.
func (w *Worker) retainAndReport(ctx context.Context, manifest attemptManifest, reason string) {
	entry := w.retainManifest(manifest, reason)
	if _, err := w.client.ReconcileWorktrees(ctx, protocol.WorktreeReconciliationReport{
		Retained: []protocol.RetainedWorktree{entry},
	}); err != nil {
		// The next registration carries the retained set again (R16); local
		// retention already holds regardless.
		w.logger.Warn("retention_ledger_report_failed",
			"attempt_id", manifest.AttemptID, "error", err)
	}
}
