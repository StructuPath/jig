// repository.go — the canonical repository identity string (U3's note, U5's
// admission half).
//
// `jobs.repository` is not a display name: the retained-worktree cap's
// skip-over (R4), the publish ledger's rows (R14), and the worker's clone
// source (R17's "never clone a URL from a ticket") all match on STRING
// EQUALITY against it. One repository reachable as "github.com/Owner/Repo",
// "git@github.com:Owner/Repo.git", and "https://github.com/owner/repo" must
// therefore collapse to one string, and it must collapse at admission —
// before any row is written that another row will later be compared to.
//
// This lives in protocol because both sides speak it: the control plane
// normalizes at admission, the worker derives its clone source from the
// stored identity.
package protocol

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// NormalizeRepositoryIdentity canonicalizes a repository identity or remote
// URL into the comparable form stored on jobs, ledger rows, and publish
// records: "host/owner/repo" for remote URLs and scp-style SSH remotes,
// "file:///canonical/path" for local repositories. It is idempotent — the
// output of one call is a valid input to the next and normalizes to itself —
// which is what lets the worker re-normalize a stored identity safely.
func NormalizeRepositoryIdentity(value string) (string, error) {
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
			// Symlinks are resolved, not merely cleaned: /tmp and /var are
			// symlinks on macOS, so two admissions of "the same" repository
			// would otherwise store two identities that never compare equal.
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

// CloneSourceForIdentity derives the only git source jig will read for a
// normalized identity: a local identity resolves to its path, a github.com
// identity to its HTTPS URL, and anything else is refused rather than
// guessed. Admission uses it to resolve a base SHA; the worker uses the same
// derivation to clone (R17).
func CloneSourceForIdentity(identity string) (string, error) {
	identity = strings.TrimSpace(identity)
	if after, found := strings.CutPrefix(identity, "file://"); found {
		return after, nil
	}
	if filepath.IsAbs(identity) {
		return identity, nil
	}
	normalized, err := NormalizeRepositoryIdentity(identity)
	if err != nil {
		return "", err
	}
	if after, found := strings.CutPrefix(normalized, "file://"); found {
		return after, nil
	}
	if strings.HasPrefix(normalized, "github.com/") {
		return "https://" + normalized + ".git", nil
	}
	return "", fmt.Errorf("repository identity %q has no derivable clone source", identity)
}
