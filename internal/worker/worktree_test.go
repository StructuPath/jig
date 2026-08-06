// worktree_test.go — the disposal-side verification that a removed worktree
// is really gone, which only means anything if it can compare git's answer
// against the path jig owns.
package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// git reports worktree paths resolved through symlinks; the manifest holds
// the path jig built. On macOS the default temporary root makes those differ
// for every attempt (/var is a symlink to /private/var), so a string
// comparison answers "not registered" for a worktree that plainly is —
// silently turning removeWorktree's registration check into one that can
// never fail.
func TestWorktreeRegisteredComparesResolvedPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repositoryDir := filepath.Join(root, "repo.git")
	seedDir := filepath.Join(root, "seed")
	gitRun(t, "", "init", "--bare", "--initial-branch=main", repositoryDir)
	gitRun(t, "", "init", "--initial-branch=main", seedDir)
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seedDir, "add", "README.md")
	gitRun(t, seedDir, "-c", "user.name=jig-test", "-c", "user.email=jig@test", "commit", "-m", "seed")
	gitRun(t, seedDir, "remote", "add", "origin", repositoryDir)
	gitRun(t, seedDir, "push", "origin", "main")

	// The unresolved path is the one a manifest carries: jig joins it from the
	// worker data directory and never resolves it.
	worktreePath := filepath.Join(root, "worktrees", "attempt-1")
	gitRun(t, repositoryDir, "worktree", "add", "-b", "jig/test/1", worktreePath, "main")

	registered, err := worktreeRegistered(ctx, repositoryDir, worktreePath)
	if err != nil {
		t.Fatalf("worktree registered: %v", err)
	}
	if !registered {
		t.Fatalf("a live worktree at %s was reported unregistered; "+
			"the removal check would then prove nothing", worktreePath)
	}

	// And the check must still be able to say no — after a real removal, with
	// the worktree directory itself already gone.
	gitRun(t, repositoryDir, "worktree", "remove", worktreePath)
	registered, err = worktreeRegistered(ctx, repositoryDir, worktreePath)
	if err != nil {
		t.Fatalf("worktree registered after removal: %v", err)
	}
	if registered {
		t.Fatal("a removed worktree is still reported as registered")
	}
}
