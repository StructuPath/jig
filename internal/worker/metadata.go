// metadata.go — refusal of repository cache entries and attempt worktrees
// whose git metadata jig did not write. Hardened flags switch off hooks,
// fsmonitor, and the ext transport, but not what else a repository's own
// config can make git execute or redirect — a credential.helper, a remote
// pushurl / uploadpack / receivepack, a core.sshCommand, an include of a
// file nobody checked. Agents run git in worktrees whose shared metadata IS
// the cache entry, and the engine can only REPORT a changed config it cannot
// restore (engine boundary.go), so the worker checks before it trusts:
// anything outside what jig itself writes is refused, never repaired or
// deleted, and stays on disk for the operator — the same discipline as an
// origin that stops matching its registered identity.
//
// This is a check at the moment of use, not a sandbox: an agent still
// running concurrently could rewrite metadata between check and use.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// inertCacheConfig is every repository-local key jig's own clone writes
// (`init --bare`, `remote add origin`, the fixed fetch refspec) or git adds
// on its own for the platform or the operator's worktree settings, mapped to
// the values it may hold. A nil set admits any single value: the origin URL
// is checked against the registered identity separately.
var inertCacheConfig = map[string][]string{
	"core.repositoryformatversion": {"0", "1"},
	"core.filemode":                {"true", "false"},
	"core.bare":                    {"true"},
	"core.ignorecase":              {"true", "false"},
	"core.precomposeunicode":       {"true", "false"},
	"core.symlinks":                {"true", "false"},
	"extensions.objectformat":      {"sha1", "sha256"},
	"extensions.relativeworktrees": {"true"},
	"remote.origin.url":            nil,
	"remote.origin.fetch":          {"+refs/heads/*:refs/remotes/origin/*"},
}

// maxMetadataFileBytes bounds the plain reads of pointer and config files.
// Anything git or jig writes there is tiny.
const maxMetadataFileBytes = 64 << 10

// validateMetadata refuses a cache entry whose metadata could make a worker
// git command execute or redirect anything: config keys outside
// inertCacheConfig (includes are never followed, so an include is itself a
// refused key), a worktree-scoped config file, or an active hook. Config is
// listed with `git config --file`, which reads only the named file.
func (e *repoEntry) validateMetadata(ctx context.Context) error {
	refuse := func(reason string) error {
		return fmt.Errorf("repository cache entry %s refused: %s; "+
			"it is left untouched for inspection — remove the directory to let jig re-clone it", e.dir, reason)
	}
	if err := requireRealDirectory(e.dir); err != nil {
		return refuse(err.Error())
	}
	// A bare entry has neither. Run in the entry, git follows a `.git` there
	// or a commondir to another repository, whose config this check never
	// reads — so either one is refused before anything below is trusted.
	for _, pointer := range []string{".git", "commondir"} {
		if _, err := os.Lstat(filepath.Join(e.dir, pointer)); err == nil {
			return refuse(fmt.Sprintf("a %s is present at the bare entry's root", pointer))
		} else if !errors.Is(err, os.ErrNotExist) {
			return refuse(fmt.Sprintf("inspect %s: %v", pointer, err))
		}
	}
	config := filepath.Join(e.dir, "config")
	if _, err := readMetadataFile(config); err != nil {
		return refuse(err.Error())
	}
	listing, err := runGit(ctx, "", "config", "--file", config, "--no-includes", "--null", "--list")
	if err != nil {
		return refuse("config unreadable: " + err.Error())
	}
	seen := make(map[string]bool)
	for _, item := range splitNUL(listing) {
		key, value, _ := strings.Cut(item, "\n")
		allowed, known := inertCacheConfig[key]
		if !known {
			// Only a subsection is free text — credential.<url>.helper can
			// carry credentials — so a key with one is never quoted.
			if strings.Count(key, ".") != 1 {
				return refuse("a config key with a subsection is not one jig writes")
			}
			return refuse(fmt.Sprintf("config key %q is not one jig writes", key))
		}
		if seen[key] {
			return refuse(fmt.Sprintf("config key %q is set more than once", key))
		}
		seen[key] = true
		if allowed != nil && !slices.Contains(allowed, value) {
			return refuse(fmt.Sprintf("config key %q has an unexpected value", key))
		}
	}
	if !seen["remote.origin.url"] {
		return refuse("config has no origin URL")
	}
	if _, err := os.Lstat(filepath.Join(e.dir, "config.worktree")); !errors.Is(err, os.ErrNotExist) {
		return refuse("a config.worktree is present")
	}
	hooks := filepath.Join(e.dir, "hooks")
	if _, err := os.Lstat(hooks); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := requireRealDirectory(hooks); err != nil {
		return refuse(err.Error())
	}
	entries, err := os.ReadDir(hooks)
	if err != nil {
		return refuse("hooks unreadable: " + err.Error())
	}
	for _, entry := range entries {
		// git's own templates install inactive *.sample scripts; anything
		// else in hooks/ is a hook git would run.
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".sample") {
			return refuse(fmt.Sprintf("active hook %q is present", entry.Name()))
		}
	}
	return nil
}

// validateWorktree refuses an attempt worktree whose git indirection does not
// lead straight back to this entry: the `.git` file must be a plain pointer
// into <entry>/worktrees/, that admin directory's gitdir must lead back to
// this worktree, its commondir must be the `../..` git writes, and its HEAD
// must be the attempt's own branch. All are read as plain files rather than
// trusted to git, because git follows them: a re-pointed `.git` or commondir
// hands every later command — status, add, commit, push — an attacker-chosen
// repository and its config. Paths are compared resolved, since git records
// resolved paths and macOS temporary roots are not (/var -> /private/var).
// The caller holds the entry mutex.
func (e *repoEntry) validateWorktree(path, branch string) error {
	refuse := func(reason string) error {
		return fmt.Errorf("worktree %s refused: %s; it is left untouched for inspection", path, reason)
	}
	pointer, err := readMetadataFile(filepath.Join(path, ".git"))
	if err != nil {
		return refuse(err.Error())
	}
	value, ok := metadataValue(pointer)
	gitDir, found := strings.CutPrefix(value, "gitdir: ")
	if !ok || !found || !plainMetadataValue(gitDir) {
		return refuse(fmt.Sprintf(".git is not a plain gitdir pointer: %q", pointer))
	}
	if !filepath.IsAbs(gitDir) {
		// Preserve symlink/.. traversal until resolution, as git does.
		gitDir = path + string(os.PathSeparator) + gitDir
	}
	admin, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return refuse("resolve .git pointer: " + err.Error())
	}
	common, err := filepath.EvalSymlinks(e.dir)
	if err != nil {
		return refuse("resolve repository cache entry: " + err.Error())
	}
	if filepath.Dir(admin) != filepath.Join(common, "worktrees") {
		return refuse(fmt.Sprintf(".git points at %s, outside the repository cache entry %s", admin, common))
	}
	if err := requireRealDirectory(admin); err != nil {
		return refuse(err.Error())
	}
	// Any admin directory under worktrees/ is not enough: one belonging to a
	// sibling attempt hands this worktree that attempt's HEAD, branch, and
	// index. The admin's gitdir backlink — absolute, or relative to the admin
	// when git writes relative paths — must name this worktree's own `.git`.
	content, err := readMetadataFile(filepath.Join(admin, "gitdir"))
	if err != nil {
		return refuse(err.Error())
	}
	backlink, ok := metadataValue(content)
	if !ok {
		return refuse(fmt.Sprintf("the admin directory's gitdir is not a plain path: %q", content))
	}
	if !filepath.IsAbs(backlink) {
		backlink = admin + string(os.PathSeparator) + backlink
	}
	owner, err := filepath.EvalSymlinks(backlink)
	if err != nil {
		return refuse("resolve gitdir backlink: " + err.Error())
	}
	own, err := filepath.EvalSymlinks(path + string(os.PathSeparator) + ".git")
	if err != nil {
		return refuse("resolve worktree: " + err.Error())
	}
	if owner != own {
		return refuse(fmt.Sprintf(".git points at admin directory %s, whose gitdir names %s, not this worktree", admin, owner))
	}
	commondir, err := readMetadataFile(filepath.Join(admin, "commondir"))
	if err != nil {
		return refuse(err.Error())
	}
	if value, ok := metadataValue(commondir); !ok || value != "../.." {
		return refuse(fmt.Sprintf("commondir is %q, not the repository cache entry", commondir))
	}
	if _, err := os.Lstat(filepath.Join(admin, "config.worktree")); !errors.Is(err, os.ErrNotExist) {
		return refuse("a config.worktree is present")
	}
	// The pointer pair can be forged consistently across two attempts — A's
	// `.git` on B's admin and B's gitdir rewritten to name A — and A's files
	// would then commit on B's branch and index. The admin's HEAD must be this
	// attempt's own branch; attempts never detach, since publication pushes
	// the branch, not HEAD.
	if branch == "" {
		return refuse("no attempt branch to check HEAD against")
	}
	head, err := readMetadataFile(filepath.Join(admin, "HEAD"))
	if err != nil {
		return refuse(err.Error())
	}
	if value, ok := metadataValue(head); !ok || value != "ref: refs/heads/"+branch {
		return refuse(fmt.Sprintf("HEAD is %q, not the attempt branch %s", head, branch))
	}
	return nil
}

// metadataValue reads a one-line pointer file the way git does. Git strips
// only the trailing line terminators (CR and LF) from `.git` and commondir
// and keeps every other byte, spaces included, as part of the path — so
// trimming spaces here would validate `<admin>` while git opens `<admin> `.
// Git trims more from gitdir and HEAD, which is why anything still edged
// with whitespace, or carrying a terminator or NUL inside, is refused rather
// than interpreted: nothing jig or git writes looks like that, and refusing
// it leaves exactly one reading of every accepted value, git's and ours.
func metadataValue(content string) (string, bool) {
	value := strings.TrimRight(content, "\r\n")
	return value, plainMetadataValue(value)
}

func plainMetadataValue(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00")
}

// checkWorktreeMetadata validates an attempt worktree on its attempt branch,
// and the cache entry it shares, before the worker runs git inside it.
func (w *Worker) checkWorktreeMetadata(ctx context.Context, repository, path, branch string) error {
	entry, err := w.cache.entry(ctx, repository)
	if err != nil {
		return err
	}
	entry.mutex.Lock()
	defer entry.mutex.Unlock()
	return entry.validateWorktree(path, branch)
}

// readMetadataFile reads one pointer or config file that must be a regular
// file: a symlink would let the check read one file while git reads another.
func readMetadataFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxMetadataFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if len(content) > maxMetadataFileBytes {
		return "", fmt.Errorf("%s is larger than any file git writes there", filepath.Base(path))
	}
	return string(content), nil
}

func requireRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", filepath.Base(path), err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a real directory", filepath.Base(path))
	}
	return nil
}

func splitNUL(output string) []string {
	var items []string
	for _, item := range strings.Split(output, "\x00") {
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}
