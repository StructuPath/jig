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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/StructuPath/jig/internal/protocol"
)

// repoCache is the worker's managed repository cache under <data>/repos.
type repoCache struct {
	root string

	mutex   sync.Mutex
	entries map[string]*repoEntry
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
	return &repoCache{root: root, entries: make(map[string]*repoEntry)}
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
	if entry.ready {
		return entry, nil
	}
	if err := c.materializeLocked(ctx, entry, repository); err != nil {
		return nil, err
	}
	entry.ready = true
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
		// Reused entry from an earlier process: the origin must still match
		// the registered identity before anything trusts it.
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
	defer os.RemoveAll(temporary)
	buildDir := filepath.Join(temporary, "repository")
	steps := [][]string{
		{"init", "--bare", buildDir},
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

// enforceLimit bounds the cache by counting installed entries on disk, and
// removes interrupted .clone-* leftovers while it is there.
func (c *repoCache) enforceLimit() error {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return fmt.Errorf("list repository cache: %w", err)
	}
	installed := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".clone-") {
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
	identity, err := normalizeRepositoryIdentity(strings.TrimSpace(stdout))
	if err != nil {
		return fmt.Errorf("revalidate repository origin: %w", err)
	}
	if !sameRemoteIdentity(identity, e.identity) {
		return errors.New("repository origin no longer matches the registered identity")
	}
	return nil
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
// or origin URL to a comparable form: "host/owner/repo" for remote URLs and
// scp-style SSH remotes, "file:///canonical/path" for local repositories.
func normalizeRepositoryIdentity(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("repository identity is empty")
	}
	if prefix, path, found := strings.Cut(value, ":"); found && strings.Contains(prefix, "@") {
		at := strings.LastIndex(prefix, "@")
		host := prefix[at+1:]
		if host == "" || path == "" {
			return "", errors.New("SSH repository identity is malformed")
		}
		return normalizeHostPath(host, path), nil
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.Scheme != "" {
		if parsed.Scheme == "file" {
			canonical, err := filepath.EvalSymlinks(parsed.Path)
			if err != nil {
				return "", fmt.Errorf("canonicalize file repository: %w", err)
			}
			return "file://" + filepath.ToSlash(canonical), nil
		}
		if parsed.Hostname() == "" || parsed.Path == "" {
			return "", errors.New("repository identity URL is malformed")
		}
		return normalizeHostPath(parsed.Host, parsed.Path), nil
	}
	if filepath.IsAbs(value) {
		canonical, err := filepath.EvalSymlinks(value)
		if err != nil {
			return "", fmt.Errorf("canonicalize repository path: %w", err)
		}
		return "file://" + filepath.ToSlash(canonical), nil
	}
	// Bare host/path identities like "github.com/owner/repo".
	if host, path, found := strings.Cut(value, "/"); found && strings.Contains(host, ".") && path != "" {
		return normalizeHostPath(host, path), nil
	}
	return "", fmt.Errorf("repository identity %q is not a recognized form", value)
}

func normalizeHostPath(host, path string) string {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	return strings.ToLower(host) + "/" + path
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
// input. Local identities clone from their path; github.com identities
// clone over HTTPS; anything else is refused.
func cloneSourceForIdentity(repository string) (string, error) {
	repository = strings.TrimSpace(repository)
	if strings.HasPrefix(repository, "file://") {
		return strings.TrimPrefix(repository, "file://"), nil
	}
	if filepath.IsAbs(repository) {
		return repository, nil
	}
	identity, err := normalizeRepositoryIdentity(repository)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(identity, "file://") {
		return strings.TrimPrefix(identity, "file://"), nil
	}
	if strings.HasPrefix(identity, "github.com/") {
		return "https://" + identity + ".git", nil
	}
	return "", fmt.Errorf("repository identity %q has no derivable clone source", repository)
}

func cacheDirectoryName(identityKey string) string {
	sum := sha256.Sum256([]byte(identityKey))
	return hex.EncodeToString(sum[:12])
}

// ---- git command runner ----------------------------------------------------

// runGit runs one git command with a bounded timeout and no terminal
// prompting, returning stdout. Failures carry a stderr excerpt.
func runGit(ctx context.Context, dir string, arguments ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.GitCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", arguments...)
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
