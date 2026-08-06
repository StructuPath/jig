// boundary_test.go — the write-boundary mechanism in isolation (R10):
// pattern semantics, fingerprint diffing (reverts count), and rollback that
// only ever undoes what the agent introduced.
package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initBoundaryRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "--quiet")
	git("config", "user.email", "test@jig.local")
	git("config", "user.name", "jig test")
	write(t, dir, "tracked.txt", "original\n")
	write(t, dir, "docs/guide.md", "guide\n")
	git("add", ".")
	git("commit", "--quiet", "-m", "init")
	return dir
}

func write(t *testing.T, dir, path, content string) {
	t.Helper()
	target := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMatchesPatternSemantics(t *testing.T) {
	cases := []struct {
		path, pattern string
		want          bool
	}{
		// Trailing slash is a directory prefix.
		{"src/a.go", "src/", true},
		{"src/deep/b.go", "src/", true},
		{"srcx/a.go", "src/", false},
		// `*` never crosses a separator.
		{"adws/adw_plan.py", "adws/adw_*.py", true},
		{"adws/data/sessions/x.py", "adws/adw_*.py", false},
		// `**` is the way to cross directories.
		{"adws/data/sessions/x.py", "adws/**.py", true},
		// `?` matches one non-separator character.
		{"a1.txt", "a?.txt", true},
		{"a/b.txt", "a?.txt", false},
		// No glob characters means exact match.
		{"notes.md", "notes.md", true},
		{"notes.md.bak", "notes.md", false},
	}
	for _, c := range cases {
		if got := matchesPattern(c.path, c.pattern); got != c.want {
			t.Errorf("matchesPattern(%q, %q) = %v, want %v", c.path, c.pattern, got, c.want)
		}
	}
}

func TestWritePermittedDistinguishesNilFromEmpty(t *testing.T) {
	if !writePermitted("anything.txt", nil) {
		t.Error("a nil allowlist must be unrestricted")
	}
	if writePermitted("anything.txt", []string{}) {
		t.Error("an empty allowlist must be read-only")
	}
	if !writePermitted("src/a.go", []string{"src/"}) {
		t.Error("allowlisted directory prefix should permit")
	}
}

func TestSnapshotCountsRevertsAsChanges(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	// The operator has uncommitted work.
	write(t, dir, "tracked.txt", "operator edit\n")
	before, err := snapshotTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, dirty := before["tracked.txt"]; !dirty {
		t.Fatal("pre-dirty file missing from the snapshot")
	}
	// The agent reverts it: the path VANISHES from the diff — and a
	// reversion is a modification.
	write(t, dir, "tracked.txt", "original\n")
	write(t, dir, "new.txt", "agent file")
	after, err := snapshotTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	changed := changedPaths(before, after)
	if len(changed) != 2 || changed[0] != "new.txt" || changed[1] != "tracked.txt" {
		t.Fatalf("changedPaths = %v, want [new.txt tracked.txt]", changed)
	}
}

func TestEnforceBoundaryRollsBackOnlyWhatTheAgentIntroduced(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	// Operator dirt before the phase: stays untouched whatever happens.
	write(t, dir, "docs/guide.md", "operator draft\n")
	before, err := snapshotTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	// The agent: one permitted write, one stray untracked file, one stray
	// tracked modification.
	write(t, dir, "src/ok.go", "package ok")
	write(t, dir, "stray.txt", "stray")
	write(t, dir, "tracked.txt", "agent overwrote this\n")

	touched, breaches, err := enforceBoundary(ctx, dir, before, []string{"src/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(touched) != 1 || touched[0] != "src/ok.go" {
		t.Fatalf("touched = %v, want [src/ok.go]", touched)
	}
	if len(breaches) != 2 {
		t.Fatalf("breaches = %v, want stray.txt and tracked.txt", breaches)
	}
	if _, err := os.Stat(filepath.Join(dir, "stray.txt")); !os.IsNotExist(err) {
		t.Error("introduced untracked file was not deleted")
	}
	body, err := os.ReadFile(filepath.Join(dir, "tracked.txt"))
	if err != nil || string(body) != "original\n" {
		t.Errorf("introduced tracked change was not checked out: %q", body)
	}
	body, err = os.ReadFile(filepath.Join(dir, "docs/guide.md"))
	if err != nil || string(body) != "operator draft\n" {
		t.Errorf("operator's pre-existing dirt was not left as-is: %q", body)
	}
}

func TestRestoreSnapshotUndoesACrashedPhase(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	before, err := snapshotTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "half/done.txt", "partial")
	write(t, dir, "tracked.txt", "partial edit\n")
	if err := restoreSnapshot(ctx, dir, before); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "half/done.txt")); !os.IsNotExist(err) {
		t.Error("untracked crash debris survived the rollback")
	}
	body, err := os.ReadFile(filepath.Join(dir, "tracked.txt"))
	if err != nil || string(body) != "original\n" {
		t.Errorf("tracked crash edit survived the rollback: %q", body)
	}
	after, err := snapshotTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(changedPaths(before, after)) != 0 {
		t.Error("worktree does not match the pre-phase snapshot")
	}
}
