// homedir.go — the ephemeral agent HOME (KTD11, R21) and per-role
// environment composition (KTD10, R17). The env allowlist alone cannot block
// file-based credentials (~/.config/gh, ~/.ssh) or dotfile persistence
// (~/.gitconfig hooks), and untrusted issue text feeding prompts makes
// indirect prompt injection a live path — so each attempt's agent
// subprocesses get a jig-created HOME/XDG directory seeded with only what
// the runtime needs, destroyed with the attempt. Anything an agent writes
// "to its home" dies with the attempt; the operator's real dotfiles are
// unreachable by construction.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// HomeSeeder provisions the minimum material a runtime needs inside a fresh
// ephemeral HOME — for claude-code, its auth material, explicitly per role
// (KTD11). Nil means an empty HOME. The hook is a parameter because what
// "minimum auth" means is a deployment decision, not an engine decision.
type HomeSeeder func(home string, role string, spec protocol.RoleSpec) error

// attemptScratch is the per-attempt directory family that lives OUTSIDE the
// worktree: the ephemeral HOME and the handoff directory. Outside matters
// twice — scratch notes can never ship in the publish diff, and the write
// boundary never has to allowlist them (the tree snapshot cannot see them).
type attemptScratch struct {
	root    string
	home    string
	handoff string
}

// createScratch materializes the attempt's scratch family under root, 0700.
func createScratch(root, attemptID string) (*attemptScratch, error) {
	base := filepath.Join(root, attemptID)
	scratch := &attemptScratch{
		root:    base,
		home:    filepath.Join(base, "home"),
		handoff: filepath.Join(base, "handoff"),
	}
	for _, dir := range []string{scratch.home, scratch.handoff} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create attempt scratch: %w", err)
		}
	}
	return scratch, nil
}

// destroy removes the whole scratch family — the "destroyed with the
// attempt" half of KTD11.
func (s *attemptScratch) destroy() error {
	return os.RemoveAll(s.root)
}

// homeEnv is the variable set that points a subprocess at the ephemeral
// HOME. XDG_* are set explicitly so XDG-respecting tools cannot fall back
// to a real config dir when HOME alone is overridden.
func homeEnv(home string) []string {
	return []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
	}
}

// composeEnv builds a subprocess environment from EXACTLY the role's env
// allowlist resolved against the worker's environment, plus the ephemeral
// HOME variables (KTD10). Never the operator's full environment; the HOME
// family always wins, so an allowlisted HOME cannot re-point an agent at
// the operator's real home.
func composeEnv(base []string, allow []string) []string {
	values := make(map[string]string, len(base))
	for _, entry := range base {
		name, value, found := strings.Cut(entry, "=")
		if found && name != "" {
			values[name] = value
		}
	}
	var composed []string
	seen := make(map[string]bool, len(allow))
	for _, name := range allow {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if value, present := values[name]; present {
			composed = append(composed, name+"="+value)
		}
	}
	return composed
}

// subprocessEnv is the complete environment an agent subprocess receives:
// the resolved allowlist plus the ephemeral HOME family. HOME-family names
// are stripped from the allowlist portion first — duplicate env entries are
// ambiguous across libcs, and an allowlisted HOME must never be able to
// re-point an agent at the operator's real home.
func subprocessEnv(base []string, allow []string, home string) []string {
	homeFamily := map[string]bool{
		"HOME": true, "XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true,
		"XDG_DATA_HOME": true, "XDG_STATE_HOME": true,
	}
	var env []string
	for _, entry := range composeEnv(base, allow) {
		name, _, _ := strings.Cut(entry, "=")
		if homeFamily[name] {
			continue
		}
		env = append(env, entry)
	}
	return append(env, homeEnv(home)...)
}
