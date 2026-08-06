// boundary.go — write-boundary enforcement by tree-fingerprint diff (R10),
// the sssf:permissions.py mechanism in Go. A role's `tools` list is a
// capability list, not a sandbox: bash runs anything and write reaches any
// path, so what an agent may CHANGE is verified after the fact against the
// worktree itself. snapshot() fingerprints the tree's change-set before an
// agent phase; enforce() compares afterwards. Comparing change-sets rather
// than watching writes is what catches reverts: a path dirty before the
// phase and clean after has been reverted, and a reversion is a
// modification. Appearing, disappearing, and changing all count.
//
// A breach is not a gate violation: gates are for work an agent can be asked
// to redo, while a breached write already happened. Out-of-allowlist changes
// the agent introduced are rolled back, then the ATTEMPT aborts — never
// retried (R10).
package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// fingerprint maps each path the working tree currently differs on to a
// state token: numstat counts for tracked changes, "untracked" for new
// files. Gitignored paths never appear.
type fingerprint map[string]string

// snapshotTree fingerprints every path the worktree differs on relative to
// HEAD. Tracked files carry numstat counts so an edit to an already-dirty
// file still registers; untracked files are listed by name.
func snapshotTree(ctx context.Context, dir string) (fingerprint, error) {
	prints := make(fingerprint)
	numstat, err := runGit(ctx, dir, "diff", "HEAD", "--numstat")
	if err != nil {
		return nil, fmt.Errorf("fingerprint worktree: %w", err)
	}
	for _, line := range strings.Split(numstat, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 3 {
			path := strings.TrimSpace(fields[len(fields)-1])
			if path != "" {
				prints[path] = fields[0] + "," + fields[1]
			}
		}
	}
	untracked, err := runGit(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("fingerprint untracked files: %w", err)
	}
	for _, path := range strings.Split(untracked, "\n") {
		if path = strings.TrimSpace(path); path != "" {
			prints[path] = "untracked"
		}
	}
	return prints, nil
}

// changedPaths lists every path whose state differs between two snapshots —
// appeared, vanished, or was rewritten.
func changedPaths(before, after fingerprint) []string {
	seen := make(map[string]bool, len(before)+len(after))
	var paths []string
	for path := range before {
		seen[path] = true
	}
	for path := range after {
		seen[path] = true
	}
	for path := range seen {
		if before[path] != after[path] {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// compileGlob translates a write-allowlist pattern with `*` stopping at `/`.
// fnmatch-style crossing would quietly widen every pattern; `**` is the way
// to say "cross directories", `?` matches one non-separator character.
func compileGlob(pattern string) *regexp.Regexp {
	var out strings.Builder
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**"):
			out.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			out.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			out.WriteString("[^/]")
			i++
		default:
			out.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}
	return regexp.MustCompile("^" + out.String() + "$")
}

// matchesPattern applies one allowlist pattern: trailing `/` is a directory
// prefix, glob characters compile with `*` bounded at separators, anything
// else is an exact path.
func matchesPattern(path, pattern string) bool {
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(path, pattern)
	}
	if strings.ContainsAny(pattern, "*?") {
		return compileGlob(pattern).MatchString(path)
	}
	return path == pattern
}

// writePermitted applies the role's write allowlist (R10, sssf semantics):
// a nil list (`writes` absent) is unrestricted, an empty list is read-only,
// otherwise only matching paths may change. The handoff directory needs no
// entry — it lives outside the worktree, so it never appears in a snapshot.
func writePermitted(path string, writes []string) bool {
	if writes == nil {
		return true
	}
	for _, pattern := range writes {
		if matchesPattern(path, pattern) {
			return true
		}
	}
	return false
}

// breach is one out-of-allowlist change and what rollback did about it.
type breach struct {
	Path    string `json:"path"`
	Outcome string `json:"outcome"`
}

// rollBackPath undoes one unauthorized change, returning a word describing
// what happened. Only changes the agent INTRODUCED are undone: a path that
// was already dirty before the phase is left exactly as it is — discarding
// an operator's uncommitted work to tidy up would be the same harm this file
// exists to prevent. A pre-dirty path the agent reverted is named loudly;
// the content is not ours to reconstruct.
func rollBackPath(ctx context.Context, dir, path string, before, after fingerprint) string {
	if _, wasDirty := before[path]; wasDirty {
		if _, stillDirty := after[path]; !stillDirty {
			return "reverted-by-agent (uncommitted work lost, cannot restore)"
		}
		return "left as-is (was already modified)"
	}
	if after[path] == "untracked" {
		if err := os.Remove(filepath.Join(dir, path)); err != nil {
			return "could not delete: " + err.Error()
		}
		return "deleted"
	}
	if _, err := runGit(ctx, dir, "checkout", "--", path); err != nil {
		return "could not roll back: " + err.Error()
	}
	return "rolled back"
}

// enforceBoundary compares the tree against the pre-phase snapshot. It
// returns the paths the agent legitimately changed; when the agent
// overstepped, everything it introduced outside the allowlist is rolled
// back first and the breaches are returned for the abort path.
func enforceBoundary(
	ctx context.Context, dir string, before fingerprint, writes []string,
) (touched []string, breaches []breach, err error) {
	after, err := snapshotTree(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	changed := changedPaths(before, after)
	var permitted []string
	for _, path := range changed {
		if writePermitted(path, writes) {
			permitted = append(permitted, path)
			continue
		}
		breaches = append(breaches, breach{
			Path:    path,
			Outcome: rollBackPath(ctx, dir, path, before, after),
		})
	}
	return permitted, breaches, nil
}

// restoreSnapshot rolls the worktree back to a pre-phase snapshot after a
// crash or watchdog kill (R11): everything the dead phase introduced is
// undone the same way a breach is. Pre-existing dirt stays; what cannot be
// restored is reported, never silently accepted.
func restoreSnapshot(ctx context.Context, dir string, before fingerprint) error {
	after, err := snapshotTree(ctx, dir)
	if err != nil {
		return err
	}
	var failures []string
	for _, path := range changedPaths(before, after) {
		outcome := rollBackPath(ctx, dir, path, before, after)
		if strings.HasPrefix(outcome, "could not") || strings.HasPrefix(outcome, "reverted-by-agent") {
			failures = append(failures, path+": "+outcome)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("worktree rollback incomplete: %s", strings.Join(failures, "; "))
	}
	return nil
}

// runGit executes one git command in dir with the protocol timeout, mirroring
// the worker's discipline: a git command that cannot finish is failed, never
// waited on indefinitely.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.GitCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
