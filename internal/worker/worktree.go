// worktree.go — attempt worktree materialization and disposal (U3). Each
// attempt gets an isolated worktree from the repository cache entry, checked
// out at the job's pinned base SHA (KTD9) on the attempt-scoped branch
// jig/<job-id>/<attempt-n> (R14's naming, used from day one), under the
// worker-owned data directory. Disposal fails closed: a worktree is deleted
// only when it is provably worthless (clean at base), provably published
// (remote-ref proof), or explicitly released by the operator (R16).
package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// attemptBranch is the attempt-scoped branch name (R14).
func attemptBranch(jobID string, attemptNumber int) string {
	return fmt.Sprintf("jig/%s/%d", jobID, attemptNumber)
}

// PreparedAttempt is what U4's phase engine receives: a claimed attempt with
// an isolated worktree at the pinned base SHA, a durable manifest behind it,
// and the lease/cancellation surface. FreshenLease must be called before any
// fenced control-plane write after a possible gap (wake-safe ordering, R5).
type PreparedAttempt struct {
	Claim        protocol.Claim
	WorktreePath string
	Branch       string
	BaseSHA      string

	lease *attemptLease
}

// Cancelled is closed when a heartbeat response carries a cancellation
// request (R5). The runner observes it; the worker never kills mid-write.
func (p *PreparedAttempt) Cancelled() <-chan struct{} { return p.lease.cancelled }

// FreshenLease heartbeats if the lease has not been renewed within one
// HeartbeatInterval — the wake-safe ordering primitive: heartbeat first,
// because only a heartbeat may revive an expired-but-unswept lease (R5).
func (p *PreparedAttempt) FreshenLease(ctx context.Context) error { return p.lease.freshen(ctx) }

// prepareAttempt materializes one claimed attempt: manifest first (so a
// crash between manifest and worktree reconciles as not_created), then the
// cache entry, then the worktree at the pinned SHA.
func (w *Worker) prepareAttempt(ctx context.Context, claim *protocol.Claim, lease *attemptLease) (*PreparedAttempt, error) {
	entry, err := w.cache.entry(ctx, claim.Job.Repository)
	if err != nil {
		return nil, fmt.Errorf("acquire repository cache entry: %w", err)
	}
	worktreePath := filepath.Join(w.worktreeRoot(), claim.Attempt.ID)
	branch := attemptBranch(claim.Job.ID, claim.Attempt.AttemptNumber)
	if err := w.manifests.create(attemptManifest{
		JobID:         claim.Job.ID,
		AttemptID:     claim.Attempt.ID,
		AttemptNumber: claim.Attempt.AttemptNumber,
		Repository:    entry.identity,
		RepositoryDir: entry.dir,
		BaseSHA:       claim.Job.BaseSHA,
		WorktreePath:  worktreePath,
		Branch:        branch,
		Lifecycle:     manifestPreparing,
	}); err != nil {
		return nil, fmt.Errorf("record attempt manifest: %w", err)
	}
	if err := w.createWorktree(ctx, entry, claim.Job.BaseSHA, worktreePath, branch); err != nil {
		if _, updateErr := w.manifests.update(claim.Attempt.ID, func(manifest *attemptManifest) error {
			manifest.Lifecycle = manifestNotCreated
			manifest.RetentionReason = boundedText(err.Error(), protocol.MaxRetentionReasonBytes)
			return nil
		}); updateErr != nil {
			err = errors.Join(err, updateErr)
		}
		return nil, err
	}
	if _, err := w.manifests.update(claim.Attempt.ID, func(manifest *attemptManifest) error {
		manifest.Lifecycle = manifestWorktreeCreated
		return nil
	}); err != nil {
		return nil, err
	}
	return &PreparedAttempt{
		Claim:        *claim,
		WorktreePath: worktreePath,
		Branch:       branch,
		BaseSHA:      claim.Job.BaseSHA,
		lease:        lease,
	}, nil
}

// createWorktree adds the attempt worktree from the cache entry, serialized
// on the entry so concurrent attempts on one repository never race a fetch
// or a checkout. The result is verified at the pinned SHA before anyone
// runs in it.
func (w *Worker) createWorktree(ctx context.Context, entry *repoEntry, baseSHA, path, branch string) error {
	entry.mutex.Lock()
	defer entry.mutex.Unlock()
	if err := entry.ensureCommit(ctx, baseSHA); err != nil {
		return err
	}
	if err := os.MkdirAll(w.worktreeRoot(), 0o700); err != nil {
		return fmt.Errorf("create worktree root: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("attempt worktree path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect attempt worktree path: %w", err)
	}
	if _, err := runGit(ctx, entry.dir, "worktree", "add", "-b", branch, path, baseSHA); err != nil {
		return err
	}
	head, err := runGit(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != baseSHA {
		return errors.New("created worktree is not at the pinned base SHA")
	}
	return nil
}

// worktreeDiskState reports whether the manifest's worktree path exists as a
// real directory. A path that exists but is not a real directory is an
// integrity error, never something to delete through.
func worktreeDiskState(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect worktree path: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("worktree path is not a real directory")
	}
	return true, nil
}

// cleanupEligible decides whether a worktree is provably safe to delete
// automatically: it must be clean, and its head must either still be the
// pinned base (nothing was ever built) or be reachable from a remote ref
// (remote-ref proof of publish, R16). Remote refs are refreshed first so
// the proof is against the origin's current state, not a stale mirror.
// A non-nil error is a retention reason, not a failure.
func (w *Worker) cleanupEligible(ctx context.Context, manifest attemptManifest) error {
	entry, err := w.cache.entry(ctx, manifest.Repository)
	if err != nil {
		return fmt.Errorf("repository cache unavailable: %w", err)
	}
	entry.mutex.Lock()
	defer entry.mutex.Unlock()
	status, err := runGit(ctx, manifest.WorktreePath, "--no-optional-locks", "status", "--porcelain=v1")
	if err != nil {
		return fmt.Errorf("worktree status unavailable: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("worktree is dirty")
	}
	head, err := runGit(ctx, manifest.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("worktree head unavailable: %w", err)
	}
	head = strings.TrimSpace(head)
	if head == manifest.BaseSHA {
		return nil
	}
	if err := entry.fetch(ctx); err != nil {
		return fmt.Errorf("remote refs unavailable for publish proof: %w", err)
	}
	published, err := runGit(ctx, entry.dir,
		"for-each-ref", "--format=%(refname)", "--contains", head, "refs/remotes")
	if err != nil {
		return fmt.Errorf("publish proof unavailable: %w", err)
	}
	if strings.TrimSpace(published) == "" {
		return errors.New("worktree contains unpublished commits")
	}
	return nil
}

// removeWorktree deletes one attempt worktree through git and verifies both
// the path and the registration are gone — git reporting success is not the
// same as the worktree being gone.
func (w *Worker) removeWorktree(ctx context.Context, manifest attemptManifest, force bool) error {
	entry, err := w.cache.entry(ctx, manifest.Repository)
	if err != nil {
		return fmt.Errorf("repository cache unavailable: %w", err)
	}
	entry.mutex.Lock()
	defer entry.mutex.Unlock()
	arguments := []string{"worktree", "remove"}
	if force {
		arguments = append(arguments, "--force")
	}
	arguments = append(arguments, manifest.WorktreePath)
	if _, err := runGit(ctx, entry.dir, arguments...); err != nil {
		return err
	}
	if _, err := os.Lstat(manifest.WorktreePath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("git reported removal success but the worktree path remains")
	}
	registered, err := worktreeRegistered(ctx, entry.dir, manifest.WorktreePath)
	if err != nil {
		return err
	}
	if registered {
		return errors.New("git reported removal success but the worktree registration remains")
	}
	return nil
}

// worktreeRegistered reports whether git still lists the path as a worktree
// of the cache entry. Both sides are resolved through symlinks before they
// are compared: git reports worktree paths in resolved form, while the
// manifest holds the path jig built. On macOS those differ for every default
// temporary root (/var is a symlink to /private/var), and a string comparison
// would answer "not registered" for every worktree — turning the check that
// exists because "git reporting success is not the same as the worktree being
// gone" into one that can never fail.
//
// A path that cannot be canonicalized is reported as an error, never as an
// absence. This function is the proof half of removeWorktree, and its caller
// reads false as "the registration is gone"; a swallowed resolution failure
// would make "I could not tell" indistinguishable from "it is not there" and
// let cleanup be declared complete on no evidence at all.
func worktreeRegistered(ctx context.Context, repositoryDir, path string) (bool, error) {
	stdout, err := runGit(ctx, repositoryDir, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want, err := resolvedWorktreePath(path)
	if err != nil {
		return false, fmt.Errorf("canonicalize worktree path %s: %w", path, err)
	}
	var unresolved error
	for _, line := range strings.Split(stdout, "\n") {
		value, found := strings.CutPrefix(line, "worktree ")
		if !found {
			continue
		}
		listed := strings.TrimSpace(value)
		absolute, absErr := filepath.Abs(listed)
		if absErr != nil {
			unresolved = errors.Join(unresolved,
				fmt.Errorf("listed worktree %q: %w", listed, absErr))
			continue
		}
		resolved, resolveErr := resolvedWorktreePath(absolute)
		if resolveErr != nil {
			unresolved = errors.Join(unresolved,
				fmt.Errorf("listed worktree %s: %w", absolute, resolveErr))
			continue
		}
		if resolved == want {
			return true, nil
		}
	}
	if unresolved != nil {
		// Not matching entries that could not be canonicalized is not proof
		// that none of them is ours.
		return false, fmt.Errorf(
			"git's worktree list could not be canonicalized: %w", unresolved)
	}
	return false, nil
}

// resolvedWorktreePath canonicalizes a worktree path through its parent
// directory, so it still resolves after the worktree itself has been deleted
// — which is exactly when worktreeRegistered runs. A parent that cannot be
// resolved yields an error rather than the unresolved input: comparing an
// unresolved path against a resolved one answers "different" for paths that
// are in fact the same.
func resolvedWorktreePath(path string) (string, error) {
	path = filepath.Clean(path)
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}
