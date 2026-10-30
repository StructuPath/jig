// metadata_test.go — the worker's own git never executes metadata an agent
// could have left in a shared cache entry or an attempt worktree, and
// refuses — without repairing or deleting — anything jig did not write.
// Every fixture is a temporary local repository; the "executables" are
// marker scripts that only record that they ran.
package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// markerScript writes a shell script that records it ran and fails, and
// returns the script's path and the marker it would create.
func markerScript(t *testing.T) (script, marker string) {
	t.Helper()
	dir := t.TempDir()
	script = filepath.Join(dir, "marker.sh")
	marker = filepath.Join(dir, "ran")
	body := "#!/bin/sh\necho ran >> '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, marker
}

func requireNoMarker(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a repository-supplied command ran worker-side (marker %s: %v)", marker, err)
	}
}

func appendConfig(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// cachedWorktree materializes a real cache entry for a fresh local origin
// and adds one attempt worktree at its head, the way prepareAttempt does.
func cachedWorktree(t *testing.T) (*Worker, attemptManifest, *repoEntry, string) {
	t.Helper()
	originDir, head, identity := newOriginRepo(t)
	dataDir := t.TempDir()
	w := &Worker{config: Config{DataDir: dataDir}, cache: newRepoCache(filepath.Join(dataDir, "repos"))}
	entry, err := w.cache.entry(context.Background(), identity)
	if err != nil {
		t.Fatalf("materialize cache entry: %v", err)
	}
	path := filepath.Join(dataDir, "worktrees", "attempt-1")
	if err := w.createWorktree(context.Background(), entry, head, path, "jig/job-1/1"); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	return w, attemptManifest{
		AttemptID: "attempt-1", Repository: identity, RepositoryDir: entry.dir,
		BaseSHA: head, WorktreePath: path, Branch: "jig/job-1/1",
	}, entry, originDir
}

// A jig-built entry and worktree pass, again on reuse, even when the
// operator's global config carries a template with an active hook.
func TestAJigBuiltCacheEntryAndWorktreePassValidation(t *testing.T) {
	templateHooks := filepath.Join(t.TempDir(), "template", "hooks")
	if err := os.MkdirAll(templateHooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateHooks, "pre-commit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[init]\n\ttemplateDir = "+filepath.Dir(templateHooks)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	w, manifest, _, _ := cachedWorktree(t)
	if err := w.checkWorktreeMetadata(context.Background(), manifest.Repository, manifest.WorktreePath, manifest.Branch); err != nil {
		t.Fatalf("a jig-built worktree was refused: %v", err)
	}
	if err := w.cleanupEligible(context.Background(), manifest); err != nil {
		t.Fatalf("a clean worktree at base is not cleanup-eligible: %v", err)
	}
}

// Finding 1 itself: a core.fsmonitor command left in the shared entry's
// config — the change the engine can report but not restore — must not run
// during cleanup, and the evidence stays where it is.
func TestCleanupNeverRunsAnFsmonitorLeftInTheSharedConfig(t *testing.T) {
	w, manifest, entry, _ := cachedWorktree(t)
	script, marker := markerScript(t)
	config := filepath.Join(entry.dir, "config")
	appendConfig(t, config, "[core]\n\tfsmonitor = "+script+"\n")

	err := w.cleanupEligible(context.Background(), manifest)
	requireNoMarker(t, marker)
	if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "core.fsmonitor") {
		t.Fatalf("cleanup eligibility = %v, want a refusal naming core.fsmonitor", err)
	}
	if body, _ := os.ReadFile(config); !strings.Contains(string(body), script) {
		t.Fatal("the contaminated config was repaired; it must stay for inspection")
	}
	if _, err := os.Stat(manifest.WorktreePath); err != nil {
		t.Fatalf("the worktree was removed: %v", err)
	}
}

// An active hook in the entry is refused before any fetch could fire it.
func TestAnActiveHookInTheCacheEntryIsRefusedBeforeAFetch(t *testing.T) {
	_, manifest, entry, originDir := cachedWorktree(t)
	script, marker := markerScript(t)
	hooks := filepath.Join(entry.dir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(script, filepath.Join(hooks, "reference-transaction")); err != nil {
		t.Fatal(err)
	}
	advanceOrigin(t, originDir)

	cache := newRepoCache(filepath.Dir(entry.dir))
	reused, err := cache.entry(context.Background(), manifest.Repository)
	if err == nil {
		// Pre-fix behaviour: the entry is trusted and the fetch runs the hook.
		_ = reused.fetch(context.Background())
	}
	requireNoMarker(t, marker)
	if err == nil || !strings.Contains(err.Error(), "active hook") {
		t.Fatalf("entry = %v, want a refusal naming the active hook", err)
	}
}

// Repository-local config that would execute or redirect during a fetch,
// a push, or credential lookup is refused on reuse before it can take effect.
func TestUnsafeCacheConfigIsRefusedBeforeItTakesEffect(t *testing.T) {
	for name, text := range map[string]string{
		"uploadpack":       "[remote \"origin\"]\n\tuploadpack = %s\n",
		"pushurl":          "[remote \"origin\"]\n\tpushurl = /elsewhere.git\n",
		"credential":       "[credential]\n\thelper = !%s\n",
		"include":          "[include]\n\tpath = %s\n",
		"second fetch":     "[remote \"origin\"]\n\tfetch = +refs/*:refs/*\n",
		"worktree config":  "[extensions]\n\tworktreeConfig = true\n",
		"ssh command":      "[core]\n\tsshCommand = %s\n",
		"origin url twice": "[remote \"origin\"]\n\turl = /elsewhere.git\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, manifest, entry, originDir := cachedWorktree(t)
			script, marker := markerScript(t)
			appendConfig(t, filepath.Join(entry.dir, "config"), strings.ReplaceAll(text, "%s", script))
			advanceOrigin(t, originDir)

			cache := newRepoCache(filepath.Dir(entry.dir))
			reused, err := cache.entry(context.Background(), manifest.Repository)
			if err == nil {
				_ = reused.fetch(context.Background())
			}
			requireNoMarker(t, marker)
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("entry = %v, want a refusal", err)
			}
			if !strings.Contains(err.Error(), entry.dir) {
				t.Fatalf("refusal %q does not name the entry directory", err)
			}
		})
	}
}

// A re-pointed `.git` or commondir hands every later command another
// repository's config. Cleanup and publication both refuse before running
// git there, and nothing is deleted.
func TestARepointedWorktreeIsRefusedByCleanupAndPublication(t *testing.T) {
	repoint := map[string]func(t *testing.T, manifest attemptManifest, entry *repoEntry, evil string){
		".git": func(t *testing.T, manifest attemptManifest, _ *repoEntry, evil string) {
			pointer := filepath.Join(manifest.WorktreePath, ".git")
			if err := os.WriteFile(pointer, []byte("gitdir: "+evil+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"commondir": func(t *testing.T, manifest attemptManifest, entry *repoEntry, evil string) {
			commondir := filepath.Join(entry.dir, "worktrees", filepath.Base(manifest.WorktreePath), "commondir")
			if err := os.WriteFile(commondir, []byte(evil+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, apply := range repoint {
		t.Run(name, func(t *testing.T) {
			w, manifest, entry, _ := cachedWorktree(t)
			script, marker := markerScript(t)
			evilRoot := t.TempDir()
			gitRun(t, "", "init", "--quiet", evilRoot)
			evil := filepath.Join(evilRoot, ".git")
			appendConfig(t, filepath.Join(evil, "config"), "[core]\n\tfsmonitor = "+script+"\n")
			apply(t, manifest, entry, evil)
			if err := os.WriteFile(filepath.Join(manifest.WorktreePath, "README.md"), []byte("edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			err := w.cleanupEligible(context.Background(), manifest)
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("cleanup eligibility = %v, want a refusal", err)
			}
			target := publishTarget{
				attemptID: manifest.AttemptID, repository: manifest.Repository, branch: manifest.Branch,
				worktreePath: manifest.WorktreePath, baseSHA: manifest.BaseSHA,
			}
			_, err = w.pushAttemptBranch(context.Background(), PublishOptions{}, target, []string{"README.md"})
			if publishCode(err) != "publish_worktree_unsafe" {
				t.Fatalf("publish = %v, want publish_worktree_unsafe", err)
			}
			err = w.requireRoundCommitsDeclared(context.Background(), target, manifest.BaseSHA, nil)
			if publishCode(err) != "publish_worktree_unsafe" {
				t.Fatalf("repair round check = %v, want publish_worktree_unsafe", err)
			}
			requireNoMarker(t, marker)
			if _, err := os.Stat(filepath.Join(manifest.WorktreePath, "README.md")); err != nil {
				t.Fatalf("the refused worktree lost its content: %v", err)
			}
		})
	}
}

// A `.git` or commondir planted at the bare entry's root redirects git run
// there to a repository whose config the check never read. The entry is
// refused on its next acquisition — before a fetch or a proof read can reach
// the planted config — and the planted pointer stays for inspection.
func TestARedirectedCacheEntryRootIsRefusedBeforeGitFollowsIt(t *testing.T) {
	plant := map[string]func(entry, evil string) (string, string){
		".git": func(entry, evil string) (string, string) {
			return filepath.Join(entry, ".git"), "gitdir: " + evil + "\n"
		},
		"commondir": func(entry, evil string) (string, string) { return filepath.Join(entry, "commondir"), evil + "\n" },
	}
	for name, pointer := range plant {
		t.Run(name, func(t *testing.T) {
			w, manifest, entry, originDir := cachedWorktree(t)
			script, marker := markerScript(t)
			evil := filepath.Join(t.TempDir(), "evil.git")
			gitRun(t, "", "init", "--bare", "--quiet", evil)
			appendConfig(t, filepath.Join(evil, "config"),
				"[remote \"origin\"]\n\turl = "+originDir+"\n\tuploadpack = "+script+"\n")
			path, body := pointer(entry.dir, evil)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			advanceOrigin(t, originDir)

			reused, err := w.cache.entry(context.Background(), manifest.Repository)
			if err == nil {
				_ = reused.fetch(context.Background())
			}
			target := publishTarget{repository: manifest.Repository}
			_, proofErr := w.remoteHead(context.Background(), target, manifest.Branch)
			requireNoMarker(t, marker)
			if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), name) {
				t.Fatalf("entry = %v, want a refusal naming %s", err, name)
			}
			if publishCode(proofErr) != "publish_repository_unavailable" {
				t.Fatalf("proof read = %v, want publish_repository_unavailable", proofErr)
			}
			if kept, _ := os.ReadFile(path); string(kept) != body {
				t.Fatalf("the planted %s was changed or removed; it must stay for inspection", name)
			}
		})
	}
}

// An entry this process already materialized is still re-checked against its
// registered identity on every acquisition: an origin URL replaced since must
// not receive the push, nor answer the proof read in place of the registered
// remote.
func TestAReadyEntryWithAReplacedOriginIsRefusedForPushAndProof(t *testing.T) {
	w, manifest, entry, originDir := cachedWorktree(t)
	substitute, _, _ := newOriginRepo(t)
	gitRun(t, "", "config", "--file", filepath.Join(entry.dir, "config"), "remote.origin.url", substitute)
	if err := os.WriteFile(filepath.Join(manifest.WorktreePath, "README.md"), []byte("edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := publishTarget{
		attemptID: manifest.AttemptID, repository: manifest.Repository, branch: manifest.Branch,
		worktreePath: manifest.WorktreePath, baseSHA: manifest.BaseSHA,
	}

	_, pushErr := w.pushAttemptBranch(context.Background(), PublishOptions{}, target, []string{"README.md"})
	_, proofErr := w.remoteHead(context.Background(), target, manifest.Branch)
	for _, remote := range []string{substitute, originDir} {
		if refs := gitRun(t, "", "ls-remote", remote, "refs/heads/"+manifest.Branch); refs != "" {
			t.Fatalf("the attempt branch reached %s: %s", remote, refs)
		}
	}
	if publishCode(pushErr) != "publish_worktree_unsafe" || !strings.Contains(pushErr.Error(), "origin") {
		t.Fatalf("publish = %v, want publish_worktree_unsafe naming the origin", pushErr)
	}
	if publishCode(proofErr) != "publish_repository_unavailable" {
		t.Fatalf("proof read = %v, want publish_repository_unavailable", proofErr)
	}
	if url := gitRun(t, "", "config", "--file", filepath.Join(entry.dir, "config"), "remote.origin.url"); url != substitute {
		t.Fatalf("the replaced origin URL was repaired to %q; it must stay for inspection", url)
	}
}

// A `.git` pointing at a SIBLING attempt's admin directory still lands under
// <entry>/worktrees, but git would then stage, commit, and push on the
// sibling's branch and index. The admin's gitdir backlink must name this
// worktree, so the attempt is refused before staging and the sibling is
// untouched.
func TestAWorktreePointedAtASiblingAttemptIsRefusedBeforeStaging(t *testing.T) {
	w, manifest, entry, _ := cachedWorktree(t)
	sibling := filepath.Join(filepath.Dir(manifest.WorktreePath), "attempt-2")
	if err := w.createWorktree(context.Background(), entry, manifest.BaseSHA, sibling, "jig/job-2/1"); err != nil {
		t.Fatalf("create sibling worktree: %v", err)
	}
	siblingAdmin := strings.TrimPrefix(strings.TrimSpace(string(mustRead(t, filepath.Join(sibling, ".git")))), "gitdir: ")
	if !filepath.IsAbs(siblingAdmin) {
		siblingAdmin = filepath.Join(sibling, siblingAdmin)
	}
	siblingHead := gitRun(t, sibling, "rev-parse", "refs/heads/jig/job-2/1")
	siblingIndex := mustRead(t, filepath.Join(siblingAdmin, "index"))
	if err := os.WriteFile(filepath.Join(manifest.WorktreePath, ".git"), []byte("gitdir: "+siblingAdmin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifest.WorktreePath, "README.md"), []byte("edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := publishTarget{
		attemptID: manifest.AttemptID, repository: manifest.Repository, branch: manifest.Branch,
		worktreePath: manifest.WorktreePath, baseSHA: manifest.BaseSHA,
	}

	_, pushErr := w.pushAttemptBranch(context.Background(), PublishOptions{}, target, []string{"README.md"})
	cleanupErr := w.cleanupEligible(context.Background(), manifest)
	if head := gitRun(t, sibling, "rev-parse", "refs/heads/jig/job-2/1"); head != siblingHead {
		t.Fatalf("the sibling's branch moved from %s to %s", siblingHead, head)
	}
	if index := mustRead(t, filepath.Join(siblingAdmin, "index")); string(index) != string(siblingIndex) {
		t.Fatal("the sibling's index was rewritten")
	}
	if status := gitRun(t, sibling, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("the sibling worktree is dirty:\n%s", status)
	}
	if publishCode(pushErr) != "publish_worktree_unsafe" || !strings.Contains(pushErr.Error(), "gitdir") {
		t.Fatalf("publish = %v, want publish_worktree_unsafe naming the gitdir backlink", pushErr)
	}
	if cleanupErr == nil || !strings.Contains(cleanupErr.Error(), "refused") {
		t.Fatalf("cleanup eligibility = %v, want a refusal", cleanupErr)
	}
	if err := w.checkWorktreeMetadata(context.Background(), manifest.Repository, sibling, "jig/job-2/1"); err != nil {
		t.Fatalf("the untouched sibling is refused: %v", err)
	}
}

// The backlink alone can be forged too: with A's `.git` on B's admin AND B's
// gitdir rewritten to name A, the pointer chain is consistent, but A's files
// would still run on B's HEAD, branch, and index. The admin's HEAD must name
// A's own attempt branch, so publication and cleanup refuse before staging,
// and both attempts and the forged pointers stay as they were.
func TestAMutuallySwappedWorktreeIsRefusedByItsBranch(t *testing.T) {
	w, manifest, entry, originDir := cachedWorktree(t)
	sibling := filepath.Join(filepath.Dir(manifest.WorktreePath), "attempt-2")
	siblingBranch := "jig/job-2/1"
	if err := w.createWorktree(context.Background(), entry, manifest.BaseSHA, sibling, siblingBranch); err != nil {
		t.Fatalf("create sibling worktree: %v", err)
	}
	adminOf := func(worktree string) string {
		admin := strings.TrimPrefix(strings.TrimSpace(string(mustRead(t, filepath.Join(worktree, ".git")))), "gitdir: ")
		if !filepath.IsAbs(admin) {
			admin = filepath.Join(worktree, admin)
		}
		return admin
	}
	ownAdmin, siblingAdmin := adminOf(manifest.WorktreePath), adminOf(sibling)
	branchHeads := func() string {
		return gitRun(t, entry.dir, "rev-parse", "refs/heads/"+manifest.Branch, "refs/heads/"+siblingBranch)
	}
	headsBefore := branchHeads()
	ownIndex := mustRead(t, filepath.Join(ownAdmin, "index"))
	siblingIndex := mustRead(t, filepath.Join(siblingAdmin, "index"))
	forged := map[string]string{
		filepath.Join(manifest.WorktreePath, ".git"): "gitdir: " + siblingAdmin + "\n",
		filepath.Join(siblingAdmin, "gitdir"):        filepath.Join(manifest.WorktreePath, ".git") + "\n",
	}
	for path, body := range forged {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(manifest.WorktreePath, "README.md"), []byte("edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := publishTarget{
		attemptID: manifest.AttemptID, repository: manifest.Repository, branch: manifest.Branch,
		worktreePath: manifest.WorktreePath, baseSHA: manifest.BaseSHA,
	}

	_, pushErr := w.pushAttemptBranch(context.Background(), PublishOptions{}, target, []string{"README.md"})
	roundErr := w.requireRoundCommitsDeclared(context.Background(), target, manifest.BaseSHA, nil)
	cleanupErr := w.cleanupEligible(context.Background(), manifest)
	if heads := branchHeads(); heads != headsBefore {
		t.Fatalf("attempt branches moved:\nbefore %s\nafter  %s", headsBefore, heads)
	}
	if string(mustRead(t, filepath.Join(ownAdmin, "index"))) != string(ownIndex) ||
		string(mustRead(t, filepath.Join(siblingAdmin, "index"))) != string(siblingIndex) {
		t.Fatal("an attempt's index was rewritten")
	}
	for _, branch := range []string{manifest.Branch, siblingBranch} {
		if refs := gitRun(t, "", "ls-remote", originDir, "refs/heads/"+branch); refs != "" {
			t.Fatalf("%s reached the origin: %s", branch, refs)
		}
	}
	if publishCode(pushErr) != "publish_worktree_unsafe" || !strings.Contains(pushErr.Error(), "HEAD") {
		t.Fatalf("publish = %v, want publish_worktree_unsafe naming HEAD", pushErr)
	}
	if publishCode(roundErr) != "publish_worktree_unsafe" {
		t.Fatalf("repair round check = %v, want publish_worktree_unsafe", roundErr)
	}
	if cleanupErr == nil || !strings.Contains(cleanupErr.Error(), "refused") {
		t.Fatalf("cleanup eligibility = %v, want a refusal", cleanupErr)
	}
	for path, body := range forged {
		if string(mustRead(t, path)) != body {
			t.Fatalf("forged pointer %s was repaired; it must stay for inspection", path)
		}
	}
	if string(mustRead(t, filepath.Join(manifest.WorktreePath, "README.md"))) != "edit\n" {
		t.Fatal("the refused worktree lost its content")
	}
}

// Valid layouts still pass the backlink check: a worktree reached through a
// symlinked alias of its root, and one git wrote with relative paths.
func TestOrdinaryWorktreeLayoutsPassTheBacklinkCheck(t *testing.T) {
	t.Run("symlinked alias", func(t *testing.T) {
		w, manifest, _, _ := cachedWorktree(t)
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(filepath.Dir(manifest.WorktreePath), alias); err != nil {
			t.Fatal(err)
		}
		aliased := filepath.Join(alias, filepath.Base(manifest.WorktreePath))
		if err := w.checkWorktreeMetadata(context.Background(), manifest.Repository, aliased, manifest.Branch); err != nil {
			t.Fatalf("a worktree reached through an alias was refused: %v", err)
		}
	})
	t.Run("relative paths", func(t *testing.T) {
		w, manifest, entry, _ := cachedWorktree(t)
		path := filepath.Join(filepath.Dir(manifest.WorktreePath), "attempt-relative")
		if _, err := runGit(context.Background(), entry.dir, "-c", "worktree.useRelativePaths=true",
			"worktree", "add", "-b", "jig/job-3/1", path, manifest.BaseSHA); err != nil {
			t.Skipf("this git cannot write relative worktree paths: %v", err)
		}
		if pointer := string(mustRead(t, filepath.Join(path, ".git"))); strings.Contains(pointer, "gitdir: /") {
			t.Skipf("this git wrote an absolute pointer: %q", pointer)
		}
		if err := w.checkWorktreeMetadata(context.Background(), manifest.Repository, path, "jig/job-3/1"); err != nil {
			t.Fatalf("a relative-path worktree was refused: %v", err)
		}
	})
}

// Git keeps spaces in `.git` and commondir paths — it strips only the line
// terminators — so a pointer that differs from a valid one by a trailing space
// names a different admin or common directory. Each fixture plants that
// directory as a copy of the entry whose config runs a marker as receivepack:
// the push would execute it if the check read the pointer more loosely than
// git does. Both are refused before staging, and the planted metadata stays.
func TestAPointerShiftedByATrailingSpaceIsRefusedWhereGitWouldFollowIt(t *testing.T) {
	plant := map[string]func(t *testing.T, manifest attemptManifest, admin, evil string) (string, string){
		".git": func(t *testing.T, manifest attemptManifest, admin, evil string) (string, string) {
			shifted := admin + " "
			copyTree(t, admin, shifted)
			if err := os.WriteFile(filepath.Join(shifted, "commondir"), []byte(evil+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(manifest.WorktreePath, ".git"), "gitdir: " + shifted + "\n"
		},
		"commondir": func(t *testing.T, _ attemptManifest, admin, evil string) (string, string) {
			// "../.. " from the admin directory is <entry>/worktrees/".. ".
			if err := os.Rename(evil, filepath.Join(filepath.Dir(admin), ".. ")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(admin, "commondir"), "../.. \n"
		},
	}
	for name, apply := range plant {
		t.Run(name, func(t *testing.T) {
			w, manifest, entry, originDir := cachedWorktree(t)
			script, marker := markerScript(t)
			evil := filepath.Join(t.TempDir(), "evil.git")
			copyTree(t, entry.dir, evil)
			appendConfig(t, filepath.Join(evil, "config"), "[remote \"origin\"]\n\treceivepack = "+script+"\n")
			admin := worktreeAdmin(t, manifest.WorktreePath)
			path, body := apply(t, manifest, admin, evil)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(manifest.WorktreePath, "README.md"), []byte("edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			target := publishTarget{
				attemptID: manifest.AttemptID, repository: manifest.Repository, branch: manifest.Branch,
				worktreePath: manifest.WorktreePath, baseSHA: manifest.BaseSHA,
			}

			_, pushErr := w.pushAttemptBranch(context.Background(), PublishOptions{}, target, []string{"README.md"})
			cleanupErr := w.cleanupEligible(context.Background(), manifest)
			requireNoMarker(t, marker)
			if publishCode(pushErr) != "publish_worktree_unsafe" || !strings.Contains(pushErr.Error(), name) {
				t.Fatalf("publish = %v, want publish_worktree_unsafe naming %s", pushErr, name)
			}
			if cleanupErr == nil || !strings.Contains(cleanupErr.Error(), "refused") {
				t.Fatalf("cleanup eligibility = %v, want a refusal", cleanupErr)
			}
			if refs := gitRun(t, "", "ls-remote", originDir, "refs/heads/"+manifest.Branch); refs != "" {
				t.Fatalf("the attempt branch reached the origin: %s", refs)
			}
			if string(mustRead(t, path)) != body {
				t.Fatalf("the planted %s was repaired; it must stay for inspection", name)
			}
			if string(mustRead(t, filepath.Join(manifest.WorktreePath, "README.md"))) != "edit\n" {
				t.Fatal("the refused worktree lost its content")
			}
		})
	}
}

// Git keeps a quoted origin URL's trailing space too, so `url = "<origin> "`
// pushes to a different local repository than the registered <origin> it
// would normalize to. It is refused before normalization, neither repository
// receives the branch, and the URL stays for inspection.
func TestAnOriginURLShiftedByATrailingSpaceIsRefused(t *testing.T) {
	w, manifest, entry, originDir := cachedWorktree(t)
	shifted := originDir + " "
	copyTree(t, originDir, shifted)
	config := filepath.Join(entry.dir, "config")
	gitRun(t, "", "config", "--file", config, "remote.origin.url", shifted)
	if err := os.WriteFile(filepath.Join(manifest.WorktreePath, "README.md"), []byte("edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := publishTarget{
		attemptID: manifest.AttemptID, repository: manifest.Repository, branch: manifest.Branch,
		worktreePath: manifest.WorktreePath, baseSHA: manifest.BaseSHA,
	}

	_, pushErr := w.pushAttemptBranch(context.Background(), PublishOptions{}, target, []string{"README.md"})
	_, proofErr := w.remoteHead(context.Background(), target, manifest.Branch)
	for _, remote := range []string{shifted, originDir} {
		if refs := gitRun(t, "", "ls-remote", remote, "refs/heads/"+manifest.Branch); refs != "" {
			t.Fatalf("the attempt branch reached %q: %s", remote, refs)
		}
	}
	if publishCode(pushErr) != "publish_worktree_unsafe" || !strings.Contains(pushErr.Error(), "origin") {
		t.Fatalf("publish = %v, want publish_worktree_unsafe naming the origin", pushErr)
	}
	if publishCode(proofErr) != "publish_repository_unavailable" {
		t.Fatalf("proof read = %v, want publish_repository_unavailable", proofErr)
	}
	if raw, _ := runGit(context.Background(), "", "config", "--file", config, "remote.origin.url"); raw != shifted+"\n" {
		t.Fatalf("the shifted origin URL was repaired to %q; it must stay for inspection", raw)
	}
}

// Spaces INSIDE paths are ordinary: a data directory, worktree, and origin
// whose paths all contain spaces still pass validation and publish.
func TestPathsWithInternalSpacesPassValidationAndPublish(t *testing.T) {
	plainOrigin, head, _ := newOriginRepo(t)
	originDir := filepath.Join(t.TempDir(), "steel origin", "repo.git")
	gitRun(t, "", "clone", "--quiet", "--bare", plainOrigin, originDir)
	identity, err := normalizeRepositoryIdentity(originDir)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "jig data")
	w := &Worker{config: Config{DataDir: dataDir}, cache: newRepoCache(filepath.Join(dataDir, "repos"))}
	entry, err := w.cache.entry(context.Background(), identity)
	if err != nil {
		t.Fatalf("materialize cache entry: %v", err)
	}
	path := filepath.Join(dataDir, "worktrees", "attempt 1")
	if err := w.createWorktree(context.Background(), entry, head, path, "jig/job-1/1"); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if err := w.checkWorktreeMetadata(context.Background(), identity, path, "jig/job-1/1"); err != nil {
		t.Fatalf("a worktree with spaces in its paths was refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := publishTarget{
		attemptID: "attempt-1", repository: identity, branch: "jig/job-1/1", worktreePath: path, baseSHA: head,
	}
	if _, err := w.pushAttemptBranch(context.Background(), PublishOptions{}, target, []string{"README.md"}); err != nil {
		t.Fatalf("publish from paths with spaces: %v", err)
	}
	if refs := gitRun(t, "", "ls-remote", originDir, "refs/heads/jig/job-1/1"); refs == "" {
		t.Fatal("the attempt branch did not reach the origin")
	}
}

// Line terminators are the one thing git strips from a pointer, so a CRLF
// pointer git follows to the same admin directory still passes.
func TestACRLFTerminatedPointerStillPasses(t *testing.T) {
	w, manifest, _, _ := cachedWorktree(t)
	admin := worktreeAdmin(t, manifest.WorktreePath)
	if err := os.WriteFile(filepath.Join(manifest.WorktreePath, ".git"), []byte("gitdir: "+admin+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.checkWorktreeMetadata(context.Background(), manifest.Repository, manifest.WorktreePath, manifest.Branch); err != nil {
		t.Fatalf("a CRLF-terminated pointer was refused: %v", err)
	}
}

// worktreeAdmin is the absolute admin directory a jig-built worktree's `.git`
// names.
func worktreeAdmin(t *testing.T, worktree string) string {
	t.Helper()
	admin := strings.TrimPrefix(strings.TrimRight(string(mustRead(t, filepath.Join(worktree, ".git"))), "\n"), "gitdir: ")
	if !filepath.IsAbs(admin) {
		admin = filepath.Join(worktree, admin)
	}
	return admin
}

func copyTree(t *testing.T, source, destination string) {
	t.Helper()
	if output, err := exec.Command("cp", "-R", source, destination).CombinedOutput(); err != nil {
		t.Fatalf("copy %s: %v: %s", source, err, output)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The worker's own git ignores fsmonitor and hooks wherever it runs, even
// in a repository no validation has looked at.
func TestWorkerGitDoesNotRunRepositoryFsmonitorOrHooks(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, "", "init", "--quiet", repo)
	script, marker := markerScript(t)
	appendConfig(t, filepath.Join(repo, ".git", "config"), "[core]\n\tfsmonitor = "+script+"\n")
	if err := os.Symlink(script, filepath.Join(repo, ".git", "hooks", "pre-commit")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "status", "--porcelain=v1")
	gitRun(t, repo, "add", "a.txt")
	gitRun(t, repo, "-c", "user.name=jig-test", "-c", "user.email=jig@test", "commit", "--quiet", "-m", "a")
	requireNoMarker(t, marker)
}

// Hardening must not cost publication its credentials: the operator's
// GLOBAL credential helper is still consulted. Isolated fixture — a temporary
// global config, no system config, a local server that only ever answers
// 401, and a helper that hands out a dummy credential.
func TestWorkerGitStillUsesTheGlobalCredentialHelper(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
		response.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	marker := filepath.Join(t.TempDir(), "helper-called")
	global := filepath.Join(t.TempDir(), "gitconfig")
	helper := "!f() { echo called >> '" + marker + "'; echo username=fixture; echo password=dummy; }; f"
	if err := os.WriteFile(global, []byte("[credential]\n\thelper = \""+helper+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	if _, err := runGit(context.Background(), "", "ls-remote", server.URL+"/repo.git"); err == nil {
		t.Fatal("ls-remote against an always-401 server succeeded")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the global credential helper was not consulted: %v", err)
	}
}

// advanceOrigin pushes one more commit to the origin, so the entry's next
// fetch updates a ref.
func advanceOrigin(t *testing.T, originDir string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	gitRun(t, "", "clone", "--quiet", originDir, clone)
	gitRun(t, clone, "-c", "user.name=jig-test", "-c", "user.email=jig@test",
		"commit", "--quiet", "--allow-empty", "-m", "advance")
	gitRun(t, clone, "push", "--quiet", "origin", "HEAD:main")
}

func TestMetadataValuesKeepGitPathWhitespaceUnambiguous(t *testing.T) {
	for _, test := range []struct {
		name, content, value string
		valid                bool
	}{
		{"no terminator", "path", "path", true},
		{"LF", "path\n", "path", true},
		{"CRLF", "path\r\n", "path", true},
		{"repeated terminators", "path\r\n\r\n", "path", true},
		{"internal space", "one two\n", "one two", true},
		{"trailing space", "path \n", "path ", false},
		{"trailing tab", "path\t\n", "path\t", false},
		{"leading space", " path\n", " path", false},
		{"embedded LF", "one\ntwo\n", "one\ntwo", false},
		{"embedded CR", "one\rtwo\n", "one\rtwo", false},
		{"NUL", "path\x00\n", "path\x00", false},
		{"empty", "\r\n", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, valid := metadataValue(test.content)
			if value != test.value || valid != test.valid {
				t.Fatalf("metadata value = %q, %t; want %q, %t", value, valid, test.value, test.valid)
			}
		})
	}
}

func TestOriginRefusalDoesNotEchoEmbeddedCredentials(t *testing.T) {
	_, _, entry, _ := cachedWorktree(t)
	for _, suffix := range []string{" ", "\t", "\n", "\x01path"} {
		t.Run(fmt.Sprintf("suffix %q", suffix), func(t *testing.T) {
			raw := "https://fixture-user:fixture-password@github.com/owner/repo" + suffix
			if _, err := runGit(context.Background(), entry.dir, "config", "remote.origin.url", raw); err != nil {
				t.Fatal(err)
			}
			err := entry.validateOrigin(context.Background())
			if err == nil || !strings.Contains(err.Error(), "padding or control characters") {
				t.Fatalf("origin validation error = %v, want unsafe URL refusal", err)
			}
			if strings.Contains(err.Error(), "fixture-password") || strings.Contains(err.Error(), "fixture-user") {
				t.Fatal("origin refusal exposed URL credentials")
			}
		})
	}
}

func TestSymlinkParentTraversalCannotRedirectWorkerGit(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("relative %t", relative), func(t *testing.T) {
			w, manifest, entry, origin := cachedWorktree(t)
			script, marker := markerScript(t)
			evil := filepath.Join(t.TempDir(), "evil.git")
			copyTree(t, entry.dir, evil)
			appendConfig(t, filepath.Join(evil, "config"), "[remote \"origin\"]\n\treceivepack = "+script+"\n")
			admin := worktreeAdmin(t, manifest.WorktreePath)
			hopTarget := filepath.Join(evil, "worktrees", "hop-target")
			if err := os.MkdirAll(hopTarget, 0o755); err != nil {
				t.Fatal(err)
			}
			hop := filepath.Join(filepath.Dir(admin), "hop")
			if err := os.Symlink(hopTarget, hop); err != nil {
				t.Fatal(err)
			}
			if relative {
				var err error
				hop, err = filepath.Rel(manifest.WorktreePath, hop)
				if err != nil {
					t.Fatal(err)
				}
			}
			// Git follows the symlink before ..; cleaning first hides evil.git.
			pointer := "gitdir: " + hop + "/../" + filepath.Base(admin) + "\n"
			pointerPath := filepath.Join(manifest.WorktreePath, ".git")
			if err := os.WriteFile(pointerPath, []byte(pointer), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(manifest.WorktreePath, "README.md"), []byte("fixture edit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			target := publishTarget{
				attemptID: manifest.AttemptID, repository: manifest.Repository, branch: manifest.Branch,
				worktreePath: manifest.WorktreePath, baseSHA: manifest.BaseSHA,
			}
			_, err := w.pushAttemptBranch(context.Background(), PublishOptions{}, target, []string{"README.md"})
			requireNoMarker(t, marker)
			if publishCode(err) != "publish_worktree_unsafe" {
				t.Fatalf("publish = %v, want unsafe worktree refusal", err)
			}
			if err := w.cleanupEligible(context.Background(), manifest); err == nil {
				t.Fatal("redirected worktree was eligible for cleanup")
			}
			if got := string(mustRead(t, pointerPath)); got != pointer {
				t.Fatal("refusal changed the planted pointer")
			}
			if refs := gitRun(t, "", "ls-remote", origin, "refs/heads/"+manifest.Branch); refs != "" {
				t.Fatalf("attempt reached origin: %s", refs)
			}
		})
	}
}

// Git hands an origin in a scheme it does not implement itself to
// git-remote-<scheme> from PATH, and protocol.ext.allow=never does not cover
// that: `jig-boundary-probe://github.com/owner/repo.git` normalizes to the
// registered github.com/owner/repo yet runs a helper an agent could plant.
// The origin is refused by transport before the identity comparison, the
// helper never runs, and the URL stays for inspection.
func TestAnOriginInACustomHelperTransportIsRefusedBeforeGitDispatchesIt(t *testing.T) {
	for _, origin := range []string{
		"jig-boundary-probe://github.com/owner/repo.git",
		"jig-boundary-probe://fixture-user:fixture-password@github.com/owner/repo.git",
		"jig-boundary-probe::https://github.com/owner/repo.git",
	} {
		t.Run(origin, func(t *testing.T) {
			helperDir := t.TempDir()
			marker := filepath.Join(t.TempDir(), "ran")
			helper := "#!/bin/sh\necho ran >> '" + marker + "'\nexit 1\n"
			if err := os.WriteFile(filepath.Join(helperDir, "git-remote-jig-boundary-probe"), []byte(helper), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			dir := filepath.Join(t.TempDir(), "entry")
			gitRun(t, "", "init", "--quiet", "--bare", "--template=", dir)
			gitRun(t, dir, "remote", "add", "origin", origin)
			entry := &repoEntry{identity: "github.com/owner/repo", dir: dir}
			if err := entry.validateMetadata(context.Background()); err != nil {
				t.Fatalf("metadata refused, so the transport check is not what is tested: %v", err)
			}

			err := entry.fetch(context.Background())
			requireNoMarker(t, marker)
			if err == nil || !strings.Contains(err.Error(), "transport") {
				t.Fatalf("fetch = %v, want a transport refusal", err)
			}
			if strings.Contains(err.Error(), "fixture-user") || strings.Contains(err.Error(), "fixture-password") {
				t.Fatal("transport refusal exposed URL credentials")
			}
			if url := gitRun(t, dir, "config", "remote.origin.url"); url != origin {
				t.Fatalf("the origin URL was repaired to %q; it must stay for inspection", url)
			}
		})
	}
}

// The origins jig's clone writes, and the standard transports git implements
// itself, still pass origin validation. Nothing is fetched.
func TestStandardOriginTransportsPassOriginValidation(t *testing.T) {
	local, _, _ := newOriginRepo(t)
	for _, origin := range []string{
		"https://github.com/owner/repo.git",
		"http://github.com/owner/repo.git",
		"ssh://git@github.com/owner/repo.git",
		"git://github.com/owner/repo.git",
		"git@github.com:owner/repo.git",
		"https://github.com/owner/re::po.git",
		"ssh://git@[::1]/owner/repo.git",
		"file://" + local,
		local,
	} {
		t.Run(origin, func(t *testing.T) {
			identity, err := normalizeRepositoryIdentity(origin)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "entry")
			gitRun(t, "", "init", "--quiet", "--bare", "--template=", dir)
			gitRun(t, dir, "remote", "add", "origin", origin)
			entry := &repoEntry{identity: identity, dir: dir}
			if err := entry.validateOrigin(context.Background()); err != nil {
				t.Fatalf("a standard origin was refused: %v", err)
			}
		})
	}
}

// A local origin is a path, whatever bytes it holds: a directory named with
// `::`, or a `:` directory followed by a doubled separator, is neither a
// helper nor a scheme to the transport check. The `::` origin fetches; git
// itself reads any `://` as a URL and refuses that one ("protocol not
// supported") without a helper, so it is checked by validation alone — jig's
// own clone never writes it, since canonical paths have no `//`.
func TestLocalOriginPathsThatLookLikeTransportSyntaxPassValidation(t *testing.T) {
	plainOrigin, head, _ := newOriginRepo(t)
	base := t.TempDir()
	for _, test := range []struct {
		name, origin string
		fetch        bool
	}{
		{"double colon", filepath.Join(base, "a::b", "origin.git"), true},
		{"colon slash slash", filepath.Join(base, "c:") + "//d/origin.git", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			gitRun(t, "", "clone", "--quiet", "--bare", plainOrigin, test.origin)
			identity, err := normalizeRepositoryIdentity(test.origin)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "entry")
			gitRun(t, "", "init", "--quiet", "--bare", "--template=", dir)
			gitRun(t, dir, "remote", "add", "origin", test.origin)
			gitRun(t, dir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
			entry := &repoEntry{identity: identity, dir: dir}
			if err := entry.validateOrigin(context.Background()); err != nil {
				t.Fatalf("a local origin at %q was refused: %v", test.origin, err)
			}
			if !test.fetch {
				return
			}
			if err := entry.fetch(context.Background()); err != nil {
				t.Fatalf("a local origin at %q did not fetch: %v", test.origin, err)
			}
			if !entry.hasCommit(context.Background(), head) {
				t.Fatal("the fetch did not bring the origin's head")
			}
		})
	}
}

// Refusals of config keys and origin URLs never quote the raw text: a URL
// subsection or an origin can carry credentials.
func TestMetadataAndOriginRefusalsDoNotEchoCredentials(t *testing.T) {
	for name, text := range map[string]string{
		"unknown key subsection": "[credential \"https://fixture-user:fixture-password@github.com\"]\n\tusername = x\n",
		"known key value":        "[core]\n\tsymlinks = https://fixture-user:fixture-password@github.com\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, manifest, entry, _ := cachedWorktree(t)
			appendConfig(t, filepath.Join(entry.dir, "config"), text)
			_, err := newRepoCache(filepath.Dir(entry.dir)).entry(context.Background(), manifest.Repository)
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("entry = %v, want a refusal", err)
			}
			if strings.Contains(err.Error(), "fixture-user") || strings.Contains(err.Error(), "fixture-password") {
				t.Fatal("config refusal exposed URL credentials")
			}
		})
	}
	_, _, entry, _ := cachedWorktree(t)
	for _, origin := range []string{
		"https://fixture-user:fixture-password@github.com:badport/owner/repo.git",
		filepath.Join(t.TempDir(), "fixture-user:fixture-password@missing"),
	} {
		t.Run("malformed origin", func(t *testing.T) {
			if _, err := runGit(context.Background(), entry.dir, "config", "remote.origin.url", origin); err != nil {
				t.Fatal(err)
			}
			err := entry.validateOrigin(context.Background())
			if err == nil {
				t.Fatal("a malformed origin passed validation")
			}
			if strings.Contains(err.Error(), "fixture-user") || strings.Contains(err.Error(), "fixture-password") {
				t.Fatal("origin refusal exposed URL credentials")
			}
		})
	}
}
