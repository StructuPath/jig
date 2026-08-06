// repocache_test.go — the shared cache root is written by more than one
// materialization at a time, so its housekeeping must be able to tell this
// process's live work from a previous process's wreckage.
package worker

import (
	"os"
	"path/filepath"
	"testing"
)

// enforceLimit reclaims .clone-* leftovers from the shared cache root. Entry
// mutexes are per-identity, so a second uncached repository can be cloning
// into one of those directories right now: reclaiming it would fail that
// attempt at git init, at fetch, or at the rename of a directory that no
// longer exists — for a reason having nothing to do with its repository.
func TestEnforceLimitLeavesThisProcessesInFlightClonesAlone(t *testing.T) {
	root := t.TempDir()
	cache := newRepoCache(root)

	inFlight, err := os.MkdirTemp(root, ".clone-")
	if err != nil {
		t.Fatal(err)
	}
	cache.claimBuild(inFlight)
	// A previous process died mid-clone and left this behind; nobody owns it.
	abandoned := filepath.Join(root, ".clone-from-a-dead-process")
	if err := os.MkdirAll(filepath.Join(abandoned, "repository"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := cache.enforceLimit(); err != nil {
		t.Fatalf("enforce limit: %v", err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Fatalf("enforceLimit deleted a clone this process is still building into: %v", err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("an abandoned clone survived reclamation (err=%v)", err)
	}

	// Once the clone finishes, its directory is nobody's again.
	cache.releaseBuild(inFlight)
	if err := cache.enforceLimit(); err != nil {
		t.Fatalf("enforce limit after release: %v", err)
	}
	if _, err := os.Stat(inFlight); !os.IsNotExist(err) {
		t.Fatalf("a released clone directory was not reclaimed (err=%v)", err)
	}
}
