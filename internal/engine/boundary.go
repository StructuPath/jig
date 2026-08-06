// boundary.go — write-boundary enforcement by tree-fingerprint diff (R10),
// the sssf:permissions.py mechanism in Go. A role's `tools` list is a
// capability list, not a sandbox: bash runs anything and write reaches any
// path, so what an agent may CHANGE is verified after the fact against the
// worktree itself. snapshotTree() fingerprints the tree's change-set before an
// agent phase; enforceBoundary() compares afterwards. Comparing change-sets
// rather than watching writes is what catches reverts: a path dirty before the
// phase and clean after has been reverted, and a reversion is a
// modification. Appearing, disappearing, and changing all count.
//
// Fingerprints are CONTENT hashes, not names or diffstat counts. Names alone
// cannot see an edit to a file an earlier phase created, and diffstat counts
// cannot see an edit that adds and removes the same number of lines. Content
// can see both.
//
// The change-set is enumerated with `-z --no-renames`: git's rename notation
// (`{docs => secrets}/a.md`) is a human description, not a path — rolling one
// back fails with "pathspec did not match", and a rename INTO a forbidden
// directory can escape detection entirely when the allowlist happens to match
// the phantom token. `--no-renames` makes every rename a delete plus an add,
// both of them literal paths.
//
// Three things `git diff` cannot see are enumerated separately, because a
// role with `writes: []` can still write to all of them: gitignored paths,
// the worktree's `.git` metadata (a planted `hooks/pre-commit` or a
// `core.hooksPath` in `config` executes during jig's OWN later git commands,
// and in a linked worktree `.git` points at the SHARED cache entry, so it
// outlives the attempt), and — the other half of the same defence — jig's own
// git invocations, which run here with hooks, fsmonitor, and the ext
// transport switched off under an explicit environment.
//
// A breach is not a gate violation: gates are for work an agent can be asked
// to redo, while a breached write already happened. Out-of-allowlist changes
// the agent introduced are rolled back, then the ATTEMPT aborts — never
// retried (R10).
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// fingerprint maps each path a snapshot covers to a content-derived state
// token. A path that is absent simply has no entry.
type fingerprint map[string]string

// treeSnapshot is one point-in-time fingerprint of everything an agent phase
// could change. The two maps are deliberately separate rather than one
// namespaced map: git metadata is addressed by ABSOLUTE path (a linked
// worktree's config and hooks live in the shared cache entry, outside the
// worktree entirely) and must never be confusable with a repository path an
// allowlist pattern could match.
type treeSnapshot struct {
	// paths is worktree content keyed by repo-relative path: tracked files
	// differing from HEAD, untracked files, and ignored-but-present paths.
	paths fingerprint
	// gitMeta is repository metadata keyed by absolute path: the `.git`
	// pointer itself, `config`/`config.worktree`, and every hook — for the
	// worktree's git dir and, when it differs, the shared common dir.
	gitMeta fingerprint
}

// snapshotTree fingerprints every path the worktree differs on relative to
// HEAD, every untracked or ignored path present in it, and the repository
// metadata that can make jig's own git commands execute code.
func snapshotTree(ctx context.Context, dir string) (treeSnapshot, error) {
	snapshot := treeSnapshot{paths: make(fingerprint), gitMeta: make(fingerprint)}

	// Tracked paths differing from HEAD. `--no-renames` so a rename is a
	// delete plus an add — two literal paths — and `-z` so a path containing
	// a space, quote, or newline arrives verbatim.
	tracked, err := runGit(ctx, dir, "diff", "HEAD", "--name-only", "-z", "--no-renames")
	if err != nil {
		return treeSnapshot{}, fmt.Errorf("fingerprint worktree: %w", err)
	}
	for _, path := range splitNUL(tracked) {
		snapshot.paths[path] = fingerprintWorktreePath(dir, path)
	}

	// Untracked, non-ignored paths.
	untracked, err := runGit(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return treeSnapshot{}, fmt.Errorf("fingerprint untracked files: %w", err)
	}
	for _, path := range splitNUL(untracked) {
		snapshot.paths[path] = fingerprintWorktreePath(dir, path)
	}

	// Ignored-but-present paths. `git diff` and `--exclude-standard` are both
	// blind to these by construction, so without this pass a role with
	// `writes: []` can drop a `.env` beside the code it is reviewing and no
	// breach is ever raised. `--directory` collapses a wholly-ignored tree
	// (node_modules, target/) into ONE entry so a build cache costs one
	// stat-walk instead of hashing hundreds of megabytes per phase.
	ignored, err := runGit(ctx, dir,
		"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return treeSnapshot{}, fmt.Errorf("fingerprint ignored files: %w", err)
	}
	for _, path := range splitNUL(ignored) {
		snapshot.paths[path] = fingerprintWorktreePath(dir, path)
	}

	if err := snapshotGitMeta(ctx, dir, snapshot.gitMeta); err != nil {
		return treeSnapshot{}, err
	}
	return snapshot, nil
}

// splitNUL splits git's `-z` output, dropping the trailing empty field.
func splitNUL(output string) []string {
	var paths []string
	for _, path := range strings.Split(output, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// gitMetaFiles are the metadata names a write to which changes what jig's own
// git commands DO: `config` carries core.hooksPath / core.fsmonitor, the
// hooks directory carries the scripts themselves.
var gitMetaFiles = []string{"config", "config.worktree"}

// snapshotGitMeta fingerprints the repository metadata reachable from dir.
// Deliberately narrow: `.git` as a whole churns on every command (index,
// logs, refs), so only the entries that can cause CODE EXECUTION or re-point
// the repository are covered. Both the worktree's git dir and the shared
// common dir are covered — in a linked worktree the latter outlives the
// attempt and is the more dangerous of the two.
func snapshotGitMeta(ctx context.Context, dir string, into fingerprint) error {
	// The `.git` entry itself: a directory in a normal repo, a FILE naming
	// the shared cache entry in a linked worktree. Rewriting it re-points
	// every later jig git command at an attacker-chosen repository. Only the
	// KIND is fingerprinted when it is a directory — `.git/` churns on every
	// git command (index, logs, ORIG_HEAD) and hashing it would breach on
	// jig's own reads.
	pointer := filepath.Join(dir, ".git")
	into[pointer] = fingerprintGitPointer(pointer)

	gitDir, err := runGit(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("locate git dir: %w", err)
	}
	dirs := []string{strings.TrimSpace(gitDir)}
	if common := commonGitDir(ctx, dir); common != "" && common != dirs[0] {
		dirs = append(dirs, common)
	}
	for _, base := range dirs {
		for _, name := range gitMetaFiles {
			path := filepath.Join(base, name)
			if token := fingerprintPath(path); token != absentToken {
				into[path] = token
			}
		}
		hooks := filepath.Join(base, "hooks")
		entries, readErr := os.ReadDir(hooks)
		if readErr != nil {
			continue
		}
		for _, entry := range entries {
			path := filepath.Join(hooks, entry.Name())
			into[path] = fingerprintPath(path)
		}
	}
	return nil
}

// commonGitDir resolves the SHARED git directory a linked worktree points
// at — the one whose config and hooks outlive this attempt. Returns "" when
// git cannot answer (an older git without `--path-format`, say): the
// worktree's own git dir is always covered regardless.
func commonGitDir(ctx context.Context, dir string) string {
	if absolute, err := runGit(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		return strings.TrimSpace(absolute)
	}
	relative, err := runGit(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(relative)
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path)
}

// fingerprintGitPointer covers the `.git` entry by KIND plus, when it is the
// linked-worktree pointer FILE, its content — which names the shared cache
// entry every later git command resolves through.
func fingerprintGitPointer(path string) string {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return absentToken
	case info.IsDir():
		return "gitdir:directory"
	default:
		return "gitdir:" + fingerprintPath(path)
	}
}

// absentToken is the state of a path that is not there. It is a token rather
// than a missing key only where a path is enumerated unconditionally.
const absentToken = "absent"

// fingerprintWorktreePath fingerprints one git-reported worktree path. A
// collapsed ignored directory arrives from `ls-files --directory` with a
// trailing slash, which filepath.Join would silently drop.
func fingerprintWorktreePath(dir, path string) string {
	if strings.HasSuffix(path, "/") {
		return fingerprintTree(filepath.Join(dir, path))
	}
	return fingerprintPath(filepath.Join(dir, path))
}

// fingerprintPath reduces one path to a token that changes whenever its
// CONTENT, mode, or kind changes. A name-and-count token cannot see a rewrite
// of a file an earlier phase created, nor an edit that adds and removes the
// same number of lines; a content hash sees both.
func fingerprintPath(absolute string) string {
	info, err := os.Lstat(absolute)
	if err != nil {
		return absentToken
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, linkErr := os.Readlink(absolute)
		if linkErr != nil {
			return "symlink:unreadable"
		}
		return "symlink:" + target
	case info.IsDir():
		return fingerprintTree(absolute)
	case !info.Mode().IsRegular():
		return "special:" + info.Mode().String()
	}
	file, err := os.Open(absolute)
	if err != nil {
		return "unreadable:" + err.Error()
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "unreadable:" + err.Error()
	}
	return fmt.Sprintf("blob:%s:%04o", hex.EncodeToString(digest.Sum(nil)), info.Mode().Perm())
}

// fingerprintTree summarizes a whole directory by walking its metadata —
// name, mode, size, mtime — without reading content. Only collapsed ignored
// trees reach here (build caches, node_modules): the question they have to
// answer is "did the role write in there at all", and paying a content hash
// per phase for half a gigabyte of dependencies to answer it is not a trade
// worth making.
func fingerprintTree(absolute string) string {
	digest := sha256.New()
	err := filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			fmt.Fprintf(digest, "%s\x00walk-error\x00", path)
			return nil //nolint:nilerr // an unreadable entry is recorded, never fatal
		}
		info, statErr := entry.Info()
		if statErr != nil {
			fmt.Fprintf(digest, "%s\x00stat-error\x00", path)
			return nil
		}
		fmt.Fprintf(digest, "%s\x00%d\x00%d\x00%d\x00",
			path, info.Mode(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return "tree:unreadable"
	}
	return "tree:" + hex.EncodeToString(digest.Sum(nil))
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

// literalPathspec disables git's pathspec magic for one path. A file named
// `a*b.txt` or `:weird` is a path, never a pattern, and never a magic prefix.
func literalPathspec(path string) string { return ":(literal)" + path }

// existsInHEAD reports whether HEAD knows the path — the question that
// decides whether rolling back means "restore" or "remove".
func existsInHEAD(ctx context.Context, dir, path string) bool {
	_, err := runGit(ctx, dir, "cat-file", "-e", "HEAD:"+path)
	return err == nil
}

// stagedInIndex reports whether the index knows the path. A breach the agent
// also STAGED must be removed from the index too, or the next phase's diff
// carries it forward.
func stagedInIndex(ctx context.Context, dir, path string) bool {
	_, err := runGit(ctx, dir, "ls-files", "--error-unmatch", "--", literalPathspec(path))
	return err == nil
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
	// Restore from HEAD, never from the index: `git checkout -- path` would
	// happily restore a breach the agent had staged.
	if existsInHEAD(ctx, dir, path) {
		if _, err := runGit(ctx, dir, "checkout", "HEAD", "--", literalPathspec(path)); err != nil {
			return "could not roll back: " + err.Error()
		}
		return "rolled back"
	}
	if stagedInIndex(ctx, dir, path) {
		if _, err := runGit(ctx, dir, "rm", "--force", "--quiet", "--", literalPathspec(path)); err != nil {
			return "could not delete: " + err.Error()
		}
		return "deleted"
	}
	target := filepath.Join(dir, strings.TrimSuffix(path, "/"))
	if err := os.RemoveAll(target); err != nil {
		return "could not delete: " + err.Error()
	}
	return "deleted"
}

// rollBackGitMeta undoes a repository-metadata write. Anything the agent
// INTRODUCED — a hook that was not there — is removed, because leaving it is
// leaving code that runs on jig's next git command in a directory that may
// outlive this attempt. Anything that existed and CHANGED cannot be
// reconstructed from here and is reported as unrestored; either way the
// attempt aborts.
func rollBackGitMeta(path string, before fingerprint) string {
	if _, existed := before[path]; existed {
		return "git metadata changed (cannot restore) — attempt aborted"
	}
	if err := os.RemoveAll(path); err != nil {
		return "could not delete planted git metadata: " + err.Error()
	}
	return "deleted"
}

// enforceBoundary compares the tree against the pre-phase snapshot. It
// returns the paths the agent legitimately changed; when the agent
// overstepped, everything it introduced outside the allowlist is rolled
// back first and the breaches are returned for the abort path.
func enforceBoundary(
	ctx context.Context, dir string, before treeSnapshot, writes []string,
) (touched []string, breaches []breach, err error) {
	after, err := snapshotTree(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	for _, path := range changedPaths(before.paths, after.paths) {
		if writePermitted(path, writes) {
			touched = append(touched, path)
			continue
		}
		breaches = append(breaches, breach{
			Path:    path,
			Outcome: rollBackPath(ctx, dir, path, before.paths, after.paths),
		})
	}
	// Repository metadata is outside every allowlist by construction:
	// `writes` names repository CONTENT, and no definition may hand a role
	// the ability to install a hook or re-point the gitdir — not even
	// `writes: ["**"]`.
	for _, path := range changedPaths(before.gitMeta, after.gitMeta) {
		breaches = append(breaches, breach{
			Path:    path,
			Outcome: rollBackGitMeta(path, before.gitMeta),
		})
	}
	return touched, breaches, nil
}

// restoreSnapshot rolls the worktree back to a pre-phase snapshot after a
// crash or watchdog kill (R11): everything the dead phase introduced is
// undone the same way a breach is. Pre-existing dirt stays; what cannot be
// restored is reported, never silently accepted.
func restoreSnapshot(ctx context.Context, dir string, before treeSnapshot) error {
	after, err := snapshotTree(ctx, dir)
	if err != nil {
		return err
	}
	var failures []string
	for _, path := range changedPaths(before.paths, after.paths) {
		outcome := rollBackPath(ctx, dir, path, before.paths, after.paths)
		if strings.HasPrefix(outcome, "could not") || strings.HasPrefix(outcome, "reverted-by-agent") {
			failures = append(failures, path+": "+outcome)
		}
	}
	for _, path := range changedPaths(before.gitMeta, after.gitMeta) {
		outcome := rollBackGitMeta(path, before.gitMeta)
		if !strings.HasPrefix(outcome, "deleted") {
			failures = append(failures, path+": "+outcome)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("worktree rollback incomplete: %s", strings.Join(failures, "; "))
	}
	return nil
}

// gitHardening is prepended to every jig-side git invocation in this package.
// The worktree these commands run in is one an agent just wrote: a planted
// `.git/hooks/pre-commit`, a `core.fsmonitor` command, or an `ext::` remote
// would otherwise execute jig-side, with jig's privileges, during the very
// commands that are supposed to be POLICING the agent. `core.quotepath=off`
// keeps non-ASCII paths literal to match the `-z` enumeration.
var gitHardening = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=",
	"-c", "protocol.ext.allow=never",
	"-c", "core.quotepath=off",
}

// gitEnv is the EXPLICIT environment every jig-side git command runs with —
// never the operator's inherited one (KTD10). Global and system config are
// neutralized so a `~/.gitconfig` (the operator's, or one an agent wrote into
// an ephemeral HOME) cannot re-enable what gitHardening just switched off.
func gitEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	}
}

// runGit executes one hardened git command in dir with the protocol timeout,
// mirroring the worker's discipline: a git command that cannot finish is
// failed, never waited on indefinitely.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.GitCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append(append([]string(nil), gitHardening...), args...)...)
	command.Dir = dir
	command.Env = gitEnv()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
