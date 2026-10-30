// repocache.go — the bounded on-demand cache of managed repositories (U3).
// One bare repository per registered repository identity; every clone source
// is derived from the job's registered identity and from nothing else — a
// URL that arrives any other way (ticket text, prompt output) is never
// cloned. Fetches are serialized per entry and bracketed by origin
// revalidation (factory:git.go): if the entry's origin ever stops matching
// its registered identity, the entry is refused, not repaired.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/StructuPath/jig/internal/protocol"
)

// repoCache is the worker's managed repository cache under <data>/repos.
type repoCache struct {
	root string

	mutex   sync.Mutex
	entries map[string]*repoEntry
	// building holds the .clone-* directory names this process is currently
	// cloning into. Entry mutexes are per-identity, so two uncached
	// repositories materialize concurrently at capacity above one; without
	// this set each one's enforceLimit would delete the other's half-built
	// clone and fail an attempt for a reason having nothing to do with it.
	building map[string]bool
}

// repoEntry is one cached bare repository. Its mutex serializes fetches and
// worktree mutations against that repository.
type repoEntry struct {
	identity string
	dir      string

	mutex sync.Mutex
	ready bool
}

func newRepoCache(root string) *repoCache {
	return &repoCache{
		root:     root,
		entries:  make(map[string]*repoEntry),
		building: make(map[string]bool),
	}
}

// entry returns the cache entry for one registered repository identity,
// cloning it on first use. The cache is bounded by MaxCachedRepositories:
// at the cap, a new identity is an error, never an eviction of retained
// state.
func (c *repoCache) entry(ctx context.Context, repository string) (*repoEntry, error) {
	identity, err := normalizeRepositoryIdentity(repository)
	if err != nil {
		return nil, err
	}
	key := remoteIdentityComparisonKey(identity)
	c.mutex.Lock()
	entry, exists := c.entries[key]
	if !exists {
		entry = &repoEntry{
			identity: identity,
			dir:      filepath.Join(c.root, cacheDirectoryName(key)),
		}
		c.entries[key] = entry
	}
	c.mutex.Unlock()

	entry.mutex.Lock()
	defer entry.mutex.Unlock()
	if !entry.ready {
		if err := c.materializeLocked(ctx, entry, repository); err != nil {
			return nil, err
		}
		entry.ready = true
	}
	// Every use, not only the first: agents run git in worktrees whose
	// metadata IS this entry, so a ready entry can have been rewritten since —
	// including its origin URL, which the push and the proof read both use.
	if err := entry.validateMetadata(ctx); err != nil {
		return nil, err
	}
	if err := entry.validateOrigin(ctx); err != nil {
		return nil, err
	}
	return entry, nil
}

func (c *repoCache) materializeLocked(ctx context.Context, entry *repoEntry, repository string) error {
	if err := os.MkdirAll(c.root, 0o700); err != nil {
		return fmt.Errorf("create repository cache root: %w", err)
	}
	if info, err := os.Lstat(entry.dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("repository cache entry must be a real directory")
		}
		// Reused entry from an earlier process: its metadata must be inert and
		// its origin must still match the registered identity before anything
		// trusts it.
		if err := entry.validateMetadata(ctx); err != nil {
			return err
		}
		return entry.validateOrigin(ctx)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect repository cache entry: %w", err)
	}
	if err := c.enforceLimit(); err != nil {
		return err
	}
	source, err := cloneSourceForIdentity(repository)
	if err != nil {
		return err
	}
	// Build in a temporary sibling and rename into place, so a crash mid-clone
	// leaves a removable .clone-* directory, never a half-real entry.
	temporary, err := os.MkdirTemp(c.root, ".clone-")
	if err != nil {
		return fmt.Errorf("create temporary clone directory: %w", err)
	}
	c.claimBuild(temporary)
	defer func() {
		c.releaseBuild(temporary)
		_ = os.RemoveAll(temporary)
	}()
	buildDir := filepath.Join(temporary, "repository")
	// No template: an operator's init.templateDir hooks would be active hooks
	// in a jig-managed entry, which validateMetadata refuses.
	steps := [][]string{
		{"init", "--bare", "--template=", buildDir},
		{"-C", buildDir, "remote", "add", "origin", source},
		{"-C", buildDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
		{"-C", buildDir, "fetch", "--no-tags", "origin"},
	}
	for _, arguments := range steps {
		if _, err := runGit(ctx, "", arguments...); err != nil {
			return err
		}
	}
	built := &repoEntry{identity: entry.identity, dir: buildDir}
	if err := built.validateOrigin(ctx); err != nil {
		return fmt.Errorf("validate cloned repository: %w", err)
	}
	if err := os.Rename(buildDir, entry.dir); err != nil {
		return fmt.Errorf("install repository cache entry: %w", err)
	}
	return nil
}

// claimBuild and releaseBuild bracket one in-flight clone, so a concurrent
// enforceLimit can tell this process's live temporary directory from a
// previous process's abandoned one.
func (c *repoCache) claimBuild(temporary string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.building[filepath.Base(temporary)] = true
}

func (c *repoCache) releaseBuild(temporary string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	delete(c.building, filepath.Base(temporary))
}

func (c *repoCache) buildInFlight(name string) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.building[name]
}

// enforceLimit bounds the cache by counting installed entries on disk, and
// reclaims interrupted .clone-* leftovers while it is there. "Interrupted"
// means a previous process's: a clone this process is building into is
// skipped, because deleting it would break a concurrent materialization of a
// different repository under its own entry mutex.
func (c *repoCache) enforceLimit() error {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return fmt.Errorf("list repository cache: %w", err)
	}
	installed := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".clone-") {
			if c.buildInFlight(entry.Name()) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(c.root, entry.Name())); err != nil {
				return fmt.Errorf("remove interrupted clone: %w", err)
			}
			continue
		}
		if entry.IsDir() {
			installed++
		}
	}
	if installed >= protocol.MaxCachedRepositories {
		return fmt.Errorf("repository cache limit of %d entries reached", protocol.MaxCachedRepositories)
	}
	return nil
}

// validateOrigin re-reads the entry's origin URL and requires it to match
// the registered identity (factory:git.go origin revalidation).
func (e *repoEntry) validateOrigin(ctx context.Context) error {
	stdout, err := runGit(ctx, e.dir, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("revalidate repository origin: %w", err)
	}
	// get-url prints the URL and one newline. Normalization trims the rest,
	// but git does not: a quoted `url = "/path/origin "` names a different
	// local repository — whose hooks a push runs — than the registered
	// /path/origin it would normalize to. A URL edged with whitespace is
	// nothing jig's clone writes, so it is refused before normalization.
	raw := strings.TrimSuffix(stdout, "\n")
	if !plainMetadataValue(raw) || strings.ContainsFunc(raw, unicode.IsControl) {
		return errors.New("revalidate repository origin: origin URL contains padding or control characters")
	}
	// Normalization reads any scheme as host/path, but git hands a scheme it
	// does not implement — and any `<helper>::<address>` — to git-remote-<name>
	// from PATH, which protocol.ext.allow does not cover. Only the transports
	// git implements itself are admitted, before the identity comparison.
	if !nativeOriginTransport(raw) {
		return errors.New("revalidate repository origin: origin URL uses an unsupported transport")
	}
	// Neither the URL nor the normalization error is quoted: an origin can
	// carry credentials.
	identity, err := normalizeRepositoryIdentity(raw)
	if err != nil {
		return errors.New("revalidate repository origin: origin URL is not a recognized repository form")
	}
	if !sameRemoteIdentity(identity, e.identity) {
		return errors.New("repository origin no longer matches the registered identity")
	}
	return nil
}

// nativeOriginTransports are the URL schemes git implements itself (remote
// curl ships as git-remote-http/https in git's own exec path).
var nativeOriginTransports = []string{"https", "http", "ssh", "git", "file"}

// nativeOriginTransport reports whether an origin URL is one git transports
// without dispatching a remote helper: an absolute local path, a URL in a
// native scheme, or scp-style user@host:path. An absolute path is checked
// first, so `::` or `://` in its bytes is not read as helper or scheme syntax
// (git itself refuses a path holding `://`, with no helper), and a native
// URL may hold `::` in an IPv6 host or its path, so only what is left
// is refused for helper syntax; `helper::https://` fails as an unknown
// scheme. Scheme case is not folded — git would look up git-remote-HTTPS —
// and anything else, relative paths included, is refused rather than
// interpreted.
func nativeOriginTransport(raw string) bool {
	if filepath.IsAbs(raw) {
		return true
	}
	if scheme, _, found := strings.Cut(raw, "://"); found {
		return slices.Contains(nativeOriginTransports, scheme)
	}
	if strings.Contains(raw, "::") {
		return false
	}
	prefix, _, found := strings.Cut(raw, ":")
	return found && strings.Contains(prefix, "@") && !strings.Contains(prefix, "/")
}

// fetch updates remote-tracking refs, with origin revalidation before and
// after: a fetch from a swapped origin must fail, and an origin swapped
// during the fetch must be detected before anything trusts the refs.
func (e *repoEntry) fetch(ctx context.Context) error {
	if err := e.validateOrigin(ctx); err != nil {
		return err
	}
	if _, err := runGit(ctx, e.dir, "fetch", "--no-tags", "--prune", "origin"); err != nil {
		return err
	}
	return e.validateOrigin(ctx)
}

// ensureCommit makes the pinned base SHA locally available, fetching at most
// once. A SHA the origin no longer serves names the escape hatch in its
// error: re-admit at head (KTD9).
func (e *repoEntry) ensureCommit(ctx context.Context, sha string) error {
	if !commitPattern.MatchString(sha) {
		return fmt.Errorf("pinned base %q is not a full commit SHA", sha)
	}
	if e.hasCommit(ctx, sha) {
		return nil
	}
	if err := e.fetch(ctx); err != nil {
		return err
	}
	if !e.hasCommit(ctx, sha) {
		return fmt.Errorf(
			"pinned base commit %s is not fetchable from %s; re-admit at head if the history moved",
			sha, e.identity)
	}
	return nil
}

func (e *repoEntry) hasCommit(ctx context.Context, sha string) bool {
	stdout, err := runGit(ctx, e.dir, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	return err == nil && strings.TrimSpace(stdout) == sha
}

// ---- identity handling -----------------------------------------------------

// normalizeRepositoryIdentity canonicalizes a registered repository identity
// or origin URL to the comparable form jobs carry. The algorithm lives in
// protocol because the control plane must produce EXACTLY this string at
// admission (U5) — the retained-worktree cap, the publish ledger, and this
// cache all match on string equality, so two implementations would be two
// chances to disagree.
func normalizeRepositoryIdentity(value string) (string, error) {
	return protocol.NormalizeRepositoryIdentity(value)
}

func remoteIdentityComparisonKey(value string) string {
	if strings.HasPrefix(strings.ToLower(value), "github.com/") {
		return strings.ToLower(value)
	}
	return value
}

func sameRemoteIdentity(left, right string) bool {
	return remoteIdentityComparisonKey(left) == remoteIdentityComparisonKey(right)
}

// cloneSourceForIdentity derives the only clone source the worker will use
// from the job's registered repository identity — never from any other
// input. Shared with admission (U5) through protocol.
func cloneSourceForIdentity(repository string) (string, error) {
	return protocol.CloneSourceForIdentity(repository)
}

func cacheDirectoryName(identityKey string) string {
	sum := sha256.Sum256([]byte(identityKey))
	return hex.EncodeToString(sum[:12])
}

// ---- git command runner ----------------------------------------------------

// workerGitHardening is prepended to every worker git command. Cleanup,
// staging, and publish run git inside worktrees agents wrote, against cache
// entries those worktrees share: a `core.fsmonitor` command, a hook, or an
// `ext::` remote left there would otherwise run worker-side. It is the
// engine's switch set minus core.quotepath, whose output change no worker
// caller expects. The environment is deliberately NOT the engine's
// sanitized one: push and fetch need the operator's global credential
// helpers, and repository-local config that could subvert those is refused
// by validateMetadata before git ever reads it.
var workerGitHardening = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=",
	"-c", "protocol.ext.allow=never",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
}

// runGit runs one hardened git command with a bounded timeout and no
// terminal prompting, returning stdout. Failures carry a stderr excerpt.
func runGit(ctx context.Context, dir string, arguments ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.GitCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git",
		append(append([]string(nil), workerGitHardening...), arguments...)...)
	if dir != "" {
		command.Dir = dir
	}
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), fmt.Errorf("git %s: %w: %s",
			strings.Join(arguments, " "), err, boundedText(detail, 1000))
	}
	return stdout.String(), nil
}
