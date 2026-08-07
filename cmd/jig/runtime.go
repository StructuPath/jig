// runtime.go — runtime selection and ephemeral-HOME auth seeding, shared by
// `jig run` and `jig worker` (U9, KTD4, KTD11).
//
// One worker process runs ONE runtime: the engine holds a single adapter per
// attempt, so which CLI a definition's roles run on is an operator choice
// (`--runtime`), not a per-role one. The probe that selection produces is the
// capability record registration and the attempt row carry (KTD4).
//
// The seeders are the KTD11 half. An agent subprocess gets a jig-created
// ephemeral HOME with no operator credential files in it, which is exactly
// why neither CLI can authenticate there on its own: Claude Code reads
// `~/.claude/.credentials.json` (or the macOS keychain), Codex reads
// `$CODEX_HOME/auth.json`. Each runtime's seeder copies the minimum material
// its CLI needs into that HOME and nothing else — no `~/.ssh`, no
// `~/.config/gh`, no `~/.gitconfig`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/StructuPath/jig/internal/engine"
	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/runtime"
	"github.com/StructuPath/jig/internal/runtime/claudecode"
	"github.com/StructuPath/jig/internal/runtime/codex"
)

// Runtime selector values. These are CLI vocabulary, deliberately equal to
// the adapters' own probe-reported names so a trace, a registration, and a
// command line all say the same word.
const (
	runtimeClaudeCode = claudecode.RuntimeName
	runtimeCodex      = codex.RuntimeName
)

// runtimeNames is the accepted `--runtime` vocabulary, for usage text and
// for the error a typo produces.
var runtimeNames = []string{runtimeClaudeCode, runtimeCodex}

// selectedRuntime is a probed adapter plus everything the callers need to
// wire it: its capability record and the auth seeder for its ephemeral HOME.
type selectedRuntime struct {
	Runtime    runtime.Runtime
	Capability protocol.RuntimeCapability
	Seeder     engine.HomeSeeder
	// Scripted reports the test hook: the enginetest fake needs no auth
	// material, so seeding is skipped for it entirely.
	Scripted bool
}

// selectRuntime builds and probes the named runtime adapter. The scripted
// test hook (JIG_SCRIPTED_RUNTIME) wins over the selector when set, because
// a test that scripts the runtime is not testing which CLI is installed.
//
// seedAuth=false leaves Seeder nil: the escape hatch for a CLI that
// authenticates through its environment instead of its HOME (an
// ANTHROPIC_API_KEY on the role's env allowlist, say).
func selectRuntime(ctx context.Context, name string, seedAuth bool) (selectedRuntime, error) {
	if scriptPath := os.Getenv(scriptedRuntimeEnv); scriptPath != "" {
		fake, err := loadScriptedRuntime(scriptPath)
		if err != nil {
			return selectedRuntime{}, err
		}
		capability, err := fake.Probe(ctx)
		if err != nil {
			return selectedRuntime{}, err
		}
		return selectedRuntime{Runtime: fake, Capability: capability, Scripted: true}, nil
	}

	var adapter runtime.Runtime
	var seeder engine.HomeSeeder
	var err error
	switch name {
	case runtimeClaudeCode:
		adapter, err = claudecode.New()
		seeder = seedClaudeAuth
	case runtimeCodex:
		adapter, err = codex.New()
		seeder = seedCodexAuth
	default:
		return selectedRuntime{}, fmt.Errorf("unknown runtime %q — expected one of %s",
			name, strings.Join(runtimeNames, ", "))
	}
	if err != nil {
		return selectedRuntime{}, err
	}
	capability, err := adapter.Probe(ctx)
	if err != nil {
		return selectedRuntime{}, err
	}
	if !seedAuth {
		seeder = nil
	}
	return selectedRuntime{Runtime: adapter, Capability: capability, Seeder: seeder}, nil
}

// seedClaudeAuth is the darwin-aware Claude Code seeder (KTD11): the minimum
// auth material the CLI needs inside the ephemeral HOME. The credentials file
// when it exists; otherwise the macOS keychain secret extracted into the
// ephemeral HOME's credentials file — the CLI's keychain lookup does not
// survive a HOME change. Onboarding state rides along so print mode skips
// first-run prompts. (Same seeding the U4 live smoke proved out.)
func seedClaudeAuth(home, _ string, _ protocol.RoleSpec) error {
	real, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		return err
	}
	credentials, readErr := os.ReadFile(filepath.Join(real, ".claude", ".credentials.json"))
	if readErr != nil {
		extracted, keychainErr := exec.Command("security",
			"find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
		if keychainErr != nil {
			return fmt.Errorf(
				"seed claude auth: no credentials file and no keychain item (%v); "+
					"use --no-seed-auth if the CLI authenticates through its environment", keychainErr)
		}
		credentials = extracted
	}
	if err := os.WriteFile(
		filepath.Join(home, ".claude", ".credentials.json"), credentials, 0o600); err != nil {
		return err
	}
	if onboarding, err := os.ReadFile(filepath.Join(real, ".claude.json")); err == nil {
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), onboarding, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// seedCodexAuth is the Codex analogue (KTD11, U10's finding): Codex reads its
// credentials from `$CODEX_HOME/auth.json`, defaulting to `~/.codex`, and
// writes its thread rollouts under `$CODEX_HOME/sessions`. Inside an
// ephemeral HOME with no CODEX_HOME on the role's env allowlist, that
// resolves to `$HOME/.codex` — this attempt's own directory — so without this
// seeder the CLI cannot authenticate at all.
//
// The location must stay STABLE across the sends of one attempt. `codex exec
// resume <thread-id>` is id-keyed, and the id resolves through the rollout
// files under `$CODEX_HOME/sessions`: a seeder that minted a fresh directory
// per send would leave every correction unable to find the thread it was told
// to continue, which the adapter reports as a session discontinuity. The
// engine seeds a role once per attempt into the attempt's single ephemeral
// HOME (engine.seedRole), so stability comes from seeding THAT directory and
// never a temporary one.
//
// A role that allowlists CODEX_HOME defeats this: the operator's own value
// would win, pointing the agent back at the real credential directory and at
// sessions that outlive the attempt. Definitions should not list it.
func seedCodexAuth(home, _ string, _ protocol.RoleSpec) error {
	source := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if source == "" {
		real, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		source = filepath.Join(real, ".codex")
	}
	target := filepath.Join(home, ".codex")
	// sessions/ is created up front: the CLI writes its rollout there on the
	// creating send, and resume reads it back on every correction.
	if err := os.MkdirAll(filepath.Join(target, "sessions"), 0o700); err != nil {
		return err
	}
	authPath := filepath.Join(source, "auth.json")
	auth, err := os.ReadFile(authPath)
	if err != nil {
		return fmt.Errorf(
			"seed codex auth: %s is unreadable (%v); run `codex login` first, "+
				"or use --no-seed-auth if the CLI authenticates through its environment",
			authPath, err)
	}
	if err := os.WriteFile(filepath.Join(target, "auth.json"), auth, 0o600); err != nil {
		return err
	}
	// config.toml rides along when the operator has one: model, provider, and
	// approval defaults live there, and a Codex started without them can pick
	// a different model than the operator's own CLI does.
	if config, err := os.ReadFile(filepath.Join(source, "config.toml")); err == nil {
		if err := os.WriteFile(filepath.Join(target, "config.toml"), config, 0o600); err != nil {
			return err
		}
	}
	return nil
}
