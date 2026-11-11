package engine

import (
	"slices"
	"strings"
	"testing"
)

// An allowlisted HUSKY must not switch the hook installer back on: the
// installer writes core.hooksPath into the cache entry's shared config, and
// the worker then refuses the entry for every later run.
func TestSubprocessEnvKeepsHuskyOffEvenWhenTheRoleAllowlistsIt(t *testing.T) {
	base := []string{"PATH=/bin", "HUSKY=1", "FOO=bar"}
	env := subprocessEnv(base, []string{"PATH", "HUSKY", "FOO"}, "/scratch/home")

	var husky []string
	for _, entry := range env {
		if strings.HasPrefix(entry, "HUSKY=") {
			husky = append(husky, entry)
		}
	}
	if !slices.Equal(husky, []string{"HUSKY=0"}) {
		t.Fatalf("HUSKY entries = %v, want exactly [HUSKY=0]", husky)
	}
	if !slices.Contains(env, "FOO=bar") || !slices.Contains(env, "HOME=/scratch/home") {
		t.Fatalf("env lost the allowlist or the ephemeral HOME: %v", env)
	}
}
