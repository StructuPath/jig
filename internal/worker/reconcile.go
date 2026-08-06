// reconcile.go — startup reconciliation and fail-closed disposal (U3, R16).
//
// At worker start, disk worktrees, manifests, and the control-plane ledger
// are compared in both directions: a manifest whose worktree survives is
// retained (never deleted on restart), a manifest whose worktree vanished
// marks its ledger row lost, and an on-disk worktree with no manifest is an
// orphan — reported to the operator, touched by nobody. U3 stops no
// processes because none exist before U4.
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
	"path/filepath"

	"github.com/StructuPath/jig/internal/protocol"
)

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
