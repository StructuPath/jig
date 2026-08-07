// repository_test.go — the canonical identity contract (U3's note, U5's
// admission half). Every row that later matches on string equality — the
// retained-worktree cap, the publish ledger, the worker's clone source —
// depends on these properties.
package protocol

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEveryFormOfOneRemoteRepositoryNormalizesToOneIdentity(t *testing.T) {
	const want = "github.com/Example/Repo"
	for _, form := range []string{
		"github.com/Example/Repo",
		"github.com/Example/Repo.git",
		"https://github.com/Example/Repo",
		"https://github.com/Example/Repo.git",
		"ssh://git@github.com/Example/Repo.git",
		"git@github.com:Example/Repo.git",
		"git@GitHub.com:Example/Repo",
	} {
		got, err := NormalizeRepositoryIdentity(form)
		if err != nil {
			t.Fatalf("normalize %q: %v", form, err)
		}
		if got != want {
			t.Fatalf("normalize %q = %q, want %q", form, got, want)
		}
	}
}

func TestANormalizedIdentityNormalizesToItself(t *testing.T) {
	directory := t.TempDir()
	for _, form := range []string{"github.com/Example/Repo", directory} {
		once, err := NormalizeRepositoryIdentity(form)
		if err != nil {
			t.Fatalf("normalize %q: %v", form, err)
		}
		twice, err := NormalizeRepositoryIdentity(once)
		if err != nil {
			t.Fatalf("re-normalize %q: %v", once, err)
		}
		if twice != once {
			t.Fatalf("normalizing %q twice gave %q then %q; the worker re-normalizes what it is handed",
				form, once, twice)
		}
	}
}

func TestALocalRepositoryNormalizesThroughItsSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	viaReal, err := NormalizeRepositoryIdentity(real)
	if err != nil {
		t.Fatal(err)
	}
	viaLink, err := NormalizeRepositoryIdentity(link)
	if err != nil {
		t.Fatal(err)
	}
	if viaReal != viaLink {
		t.Fatalf("a repository reached through a symlink normalized to %q, not %q", viaLink, viaReal)
	}
	if viaReal != "file://"+mustEvalSymlinks(t, real) {
		t.Fatalf("local identity = %q, want the file:// canonical path", viaReal)
	}
}

func TestAnUnrecognizableRepositoryIdentityIsRefusedRatherThanGuessed(t *testing.T) {
	for _, form := range []string{"", "   ", "not-a-repo", "relative/path", "/no/such/path/here"} {
		if got, err := NormalizeRepositoryIdentity(form); err == nil {
			t.Fatalf("normalize %q returned %q, want a refusal", form, got)
		}
	}
}

func TestACloneSourceIsDerivedOnlyFromTheIdentity(t *testing.T) {
	directory := t.TempDir()
	local, err := NormalizeRepositoryIdentity(directory)
	if err != nil {
		t.Fatal(err)
	}
	source, err := CloneSourceForIdentity(local)
	if err != nil {
		t.Fatal(err)
	}
	if source != mustEvalSymlinks(t, directory) {
		t.Fatalf("local clone source = %q, want the canonical path", source)
	}
	source, err = CloneSourceForIdentity("github.com/Example/Repo")
	if err != nil {
		t.Fatal(err)
	}
	if source != "https://github.com/Example/Repo.git" {
		t.Fatalf("github clone source = %q", source)
	}
	if _, err := CloneSourceForIdentity("gitlab.example.com/team/repo"); err == nil {
		t.Fatal("an identity with no derivable source must be refused, never guessed")
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(resolved)
}
