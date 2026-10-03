// tests_intact.go — the tests_intact gate (R9). A repair loop that feeds red
// tests or red CI back to a builder invites the cheapest "fix": delete the
// failing test, drop its assertion, or mark it skipped (the CI-repair plan's
// "weakening the check" risk). The risk classifier only holds such work for a
// person after the fact; this gate refutes it at the phase that wrote it, so
// the builder is told in the same session to put the check back.
//
// It is a claim verifier in the registry's sense: every agent phase claims
// to have done the task, and a change that quietly removes tests is evidence
// against that claim, read mechanically off the diff. It counts lines by
// pattern and never parses code — good enough to catch the blunt weakenings
// an agent reaches for, and cheap enough to run after every emission.
package engine

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// testPathPattern is factory.yaml's classify-risk `tests` pattern plus
// pytest's test_*.py and conftest.py (where collect_ignore hides whole
// files), so the gate and the risk classifier agree on what a test file is.
var testPathPattern = regexp.MustCompile(
	`(_test\.go|_test\.py|\.(test|spec)\.[cm]?[jt]sx?|_spec\.rb)$|(^|/)(test_[^/]*|conftest)\.py$|(^|/)(tests?|__tests__|spec)/`)

// markerPattern finds something that stops tests from running: a skip, a
// focus that silences the rest, a build constraint or collection rule that
// drops a file, an exit that ends the run early. A named `kind` group, when
// present, is what identifies the marker; otherwise the whole match does.
type markerPattern struct {
	re *regexp.Regexp
	// inComments: the marker looks like a comment (`//go:build`) and must
	// not be dropped by the comment filter.
	inComments bool
}

// testLanguage is one v1 language's line patterns. A test file in a language
// with none (a fixture under tests/, say) is still checked for deletion.
type testLanguage struct {
	comment *regexp.Regexp // a whole-line comment, ignored by the counts
	tests   *regexp.Regexp // declares one test
	asserts *regexp.Regexp // makes one assertion
	markers []markerPattern
}

var (
	langGo = &testLanguage{
		comment: regexp.MustCompile(`^\s*(//|/\*|\*)`),
		tests:   regexp.MustCompile(`^\s*func\s+Test`),
		asserts: regexp.MustCompile(`\bt\.(Error|Fatal|Fail)|\b(assert|require)\.\w+\s*\(`),
		markers: []markerPattern{
			{re: regexp.MustCompile(`\b[tb]\.Skip\w*`)},
			// A TestMain owns the run: it can return without m.Run().
			{re: regexp.MustCompile(`^\s*func\s+TestMain\s*\(`)},
			{re: regexp.MustCompile(`\bos\.Exit\s*\(`)},
			// A constraint like `//go:build ignore` drops the file from the
			// build. The whole line is the kind, so changing a constraint
			// counts as adding one.
			{re: regexp.MustCompile(`^\s*//\s*(go:build|\+build)\b.*$`), inComments: true},
		},
	}
	langJS = &testLanguage{
		comment: regexp.MustCompile(`^\s*(//|/\*|\*)`),
		tests:   regexp.MustCompile(`\b(it|test)\s*\(`),
		asserts: regexp.MustCompile(`\bexpect\s*\(|\bassert(\.\w+)?\s*\(`),
		markers: []markerPattern{
			// .only/.skip anywhere in a test-function chain, .each forms
			// included: test.only.each, describe.skip.each, it.concurrent.only.
			{re: regexp.MustCompile(`\b(describe|context|suite|it|test|specify)(\.\w+)*?\.(?P<kind>only|skip)\b`)},
			{re: regexp.MustCompile(`\b(?P<kind>xit|xdescribe|xtest|xcontext|fit|fdescribe)\s*[.(]`)},
		},
	}
	langPython = &testLanguage{
		comment: regexp.MustCompile(`^\s*#`),
		tests:   regexp.MustCompile(`^\s*(async\s+)?def\s+test`),
		asserts: regexp.MustCompile(`^\s*assert\b|\bself\.assert\w*\s*\(`),
		markers: []markerPattern{
			// `mark.skip` rather than `@pytest.mark.skip`, so the
			// parenthesized, aliased, and pytest.param(marks=…) forms count.
			{re: regexp.MustCompile(`\bmark\.(skip|skipif|xfail)\b`)},
			{re: regexp.MustCompile(`\bunittest\.(skip\w*|expectedFailure)\b`)},
			{re: regexp.MustCompile(`\bpytest\.(skip|xfail)\s*\(|\bself\.skipTest\s*\(`)},
			{re: regexp.MustCompile(`^\s*pytestmark\s*(=|\+=)`)},
			{re: regexp.MustCompile(`^\s*collect_ignore(_glob)?\s*(=|\+=|\.(append|extend|insert)\s*\()`)},
		},
	}
	langRuby = &testLanguage{
		comment: regexp.MustCompile(`^\s*#`),
		tests:   regexp.MustCompile(`^\s*(it|specify)\b`),
		asserts: regexp.MustCompile(`\bexpect\s*[({]|\bshould(_not)?\b`),
		markers: []markerPattern{
			// At a line start, or inline after `do`, `;` or `{`.
			{re: regexp.MustCompile(`(^|[;{]|\bdo\b)\s*(?P<kind>skip|pending|xit|xspecify|xdescribe|xcontext|fit|fdescribe|fcontext)\b`)},
		},
	}
	jsExtension = regexp.MustCompile(`\.[cm]?[jt]sx?$`)
)

func testLanguageOf(file string) *testLanguage {
	switch {
	case strings.HasSuffix(file, ".go"):
		return langGo
	case strings.HasSuffix(file, ".py"):
		return langPython
	case strings.HasSuffix(file, ".rb"):
		return langRuby
	case jsExtension.MatchString(file):
		return langJS
	}
	return nil
}

// diffLine is one added or removed line. For an added line, line is its
// number in the worktree; for a removed one it is where the line used to sit
// in the worktree's numbering, so the finding points at the gap.
type diffLine struct {
	line int
	text string
}

type fileDiff struct {
	path    string
	deleted bool
	// binary: git reported the file as binary, so it has no hunks to read.
	binary bool
	// untracked: a new file outside git's index; every line is added.
	untracked      bool
	added, removed []diffLine
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseUnifiedDiff reads `git diff` output. Header lines are only read
// before a file's first hunk, so a removed line that itself starts with
// "--- " is never mistaken for one; "diff --git" can only be a header,
// because every hunk line starts with a marker.
func parseUnifiedDiff(output string) []*fileDiff {
	var files []*fileDiff
	var current *fileDiff
	inHunk := false
	newLine := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			current = &fileDiff{path: diffGitPath(strings.TrimPrefix(line, "diff --git "))}
			files = append(files, current)
			inHunk = false
			continue
		}
		if current == nil {
			continue
		}
		if match := hunkHeader.FindStringSubmatch(line); match != nil {
			inHunk = true
			newLine, _ = strconv.Atoi(match[1])
			// A hunk with no new lines starts at the line BEFORE the gap;
			// its removals sit at the line after.
			if match[2] == "0" {
				newLine++
			}
			continue
		}
		if !inHunk {
			switch {
			case strings.HasPrefix(line, "deleted file mode"):
				current.deleted = true
			case strings.HasPrefix(line, "Binary files "), strings.HasPrefix(line, "GIT binary patch"):
				current.binary = true
			case strings.HasPrefix(line, "--- a/"), strings.HasPrefix(line, `--- "a/`):
				current.path = diffHeaderPath(strings.TrimPrefix(line, "--- "), "a/")
			case strings.HasPrefix(line, "+++ b/"), strings.HasPrefix(line, `+++ "b/`):
				current.path = diffHeaderPath(strings.TrimPrefix(line, "+++ "), "b/")
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			current.added = append(current.added, diffLine{newLine, line[1:]})
			newLine++
		case strings.HasPrefix(line, "-"):
			current.removed = append(current.removed, diffLine{newLine, line[1:]})
		case strings.HasPrefix(line, " "):
			// Context, should a repository's config ever add some back.
			newLine++
		}
	}
	return files
}

// diffGitPath recovers the path from a `diff --git a/P b/P` header. With
// --no-renames both sides name the same path, so an unquoted header splits
// exactly in half even when the path contains spaces.
func diffGitPath(rest string) string {
	if strings.HasPrefix(rest, `"`) {
		quoted, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return rest
		}
		return diffHeaderPath(quoted, "a/")
	}
	if len(rest) < 5 {
		return rest
	}
	return rest[2 : 2+(len(rest)-5)/2]
}

// diffHeaderPath strips a side prefix from a header path, undoing git's
// C-style quoting of unusual names first. Git ends a ---/+++ path that
// contains a space with a tab; a name holding a real tab is always quoted,
// so a literal trailing tab is never part of the name.
func diffHeaderPath(raw, prefix string) string {
	raw = strings.TrimSuffix(raw, "\t")
	if strings.HasPrefix(raw, `"`) {
		if unquoted, err := strconv.Unquote(raw); err == nil {
			raw = unquoted
		}
	}
	return strings.TrimPrefix(raw, prefix)
}

// matching returns the lines that match pattern, skipping whole-line
// comments: commenting a test out is a removal.
func (l *testLanguage) matching(lines []diffLine, pattern *regexp.Regexp) []diffLine {
	var out []diffLine
	for _, line := range lines {
		if l.comment.MatchString(line.text) || !pattern.MatchString(line.text) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// markerHit is one marker occurrence: its kind and the line it is on.
type markerHit struct {
	kind string
	line diffLine
}

// markersIn finds every marker on lines. A comment line is skipped (a
// commented-out t.Skip is no skip) unless the pattern is one that looks like
// a comment by design.
func (l *testLanguage) markersIn(lines []diffLine) []markerHit {
	var hits []markerHit
	for _, line := range lines {
		comment := l.comment.MatchString(line.text)
		for _, marker := range l.markers {
			if comment && !marker.inComments {
				continue
			}
			kindIndex := marker.re.SubexpIndex("kind")
			for _, match := range marker.re.FindAllStringSubmatch(line.text, -1) {
				kind := match[0]
				if kindIndex >= 0 {
					kind = match[kindIndex]
				}
				hits = append(hits, markerHit{strings.Join(strings.Fields(kind), ""), line})
			}
		}
	}
	return hits
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// maxUntrackedBytes bounds how much of one untracked file is read: markers
// are short lines, and an agent must not be able to make the gate slurp an
// arbitrarily large file.
const maxUntrackedBytes = 1 << 20

// gateTestsIntact: no test file changed since the pinned base may lose a
// test, lose an assertion, or gain a skip/focus marker, and none may be
// deleted — unless the definition's allow list names it. Losses are NET per
// file (removed > added), so a rename inside the file or an assertion
// replaced with a stronger one passes; a renamed FILE is a deletion, since
// --no-renames keeps the old path visible as gone.
func gateTestsIntact(gc gateContext) protocol.GateReport {
	var report protocol.GateReport
	if gc.baseSHA == "" {
		report.Check("base commit", false, "the attempt has no pinned base commit to diff against")
		return report
	}
	// Prefixes are pinned so a repository's diff.noprefix or
	// diff.mnemonicPrefix cannot change the header shape this parses;
	// external diff drivers and textconv are off, --text overrides a
	// `-diff` attribute that would collapse a file to "Binary files
	// differ", and no inter-hunk context keeps line numbers exact. The
	// worktree's own config and attributes must not decide what this reads.
	output, err := runGit(gc.ctx, gc.worktree, "diff", "--no-renames", "--no-ext-diff", "--no-textconv",
		"--text", "--no-color", "--unified=0", "--inter-hunk-context=0",
		"--src-prefix=a/", "--dst-prefix=b/", gc.baseSHA, "--")
	if err != nil {
		report.Check("git diff", false, err.Error())
		return report
	}
	files := parseUnifiedDiff(output)
	// git diff never shows untracked files, and a new test file is where a
	// `//go:build ignore` or a conftest collect_ignore is cheapest to plant.
	listed, err := runGit(gc.ctx, gc.worktree, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		report.Check("git ls-files", false, err.Error())
		return report
	}
	for _, name := range strings.Split(listed, "\x00") {
		if name == "" || !testPathPattern.MatchString(name) {
			continue
		}
		file, err := readUntracked(gc.worktree, name)
		if err != nil {
			report.Check(name, false, "untracked test file could not be read: "+err.Error())
			continue
		}
		if file != nil {
			files = append(files, file)
		}
	}
	judgeTestFiles(&report, files, gc.allow, shortSHA(gc.baseSHA))
	return report
}

// readUntracked loads an untracked file as all-added lines. Anything but a
// regular file is skipped: a symlink could point outside the worktree, and
// the gate must not echo lines from wherever it leads.
func readUntracked(worktree, name string) (*fileDiff, error) {
	full, ok := artifactPath(worktree, filepath.FromSlash(name))
	if !ok {
		return nil, fmt.Errorf("path is not inside the worktree")
	}
	info, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	handle, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	content, err := io.ReadAll(io.LimitReader(handle, maxUntrackedBytes))
	if err != nil {
		return nil, err
	}
	file := &fileDiff{path: name, untracked: true}
	for i, text := range strings.Split(string(content), "\n") {
		file.added = append(file.added, diffLine{i + 1, text})
	}
	return file, nil
}

// judgeTestFiles records one or more checks per test file in files.
func judgeTestFiles(report *protocol.GateReport, files []*fileDiff, allow []string, base string) {
	checked := 0
	for _, file := range files {
		if !testPathPattern.MatchString(file.path) {
			continue
		}
		checked++
		if glob := allowedBy(file.path, allow); glob != "" {
			report.Check(file.path, true, fmt.Sprintf("exempt: allowed by %q", glob))
			continue
		}
		if file.deleted {
			report.Check(file.path, false, fmt.Sprintf(
				"test file deleted since base %s; restore it (git checkout %s -- %s). "+
					"Removing a test is only allowed when the definition's tests_intact allow list names it",
				base, base, file.path))
			continue
		}
		if file.binary {
			// Unreachable with --text, unless git changes; a file the gate
			// cannot read is never one it vouches for.
			report.Check(file.path, false,
				"git reports this test file as binary, so its changes cannot be inspected; "+
					"restore it as text or name it in the tests_intact allow list")
			continue
		}
		lang := testLanguageOf(file.path)
		if lang == nil {
			report.Check(file.path, true, "changed, not deleted")
			continue
		}
		before := len(report.Violations())
		netLoss(report, file, lang, lang.tests, "test function(s)",
			"restore the removed tests, or rewrite each as an equivalent test; deleting a failing test does not fix the code")
		netLoss(report, file, lang, lang.asserts, "assertion(s)",
			"restore the removed assertions or replace each with one at least as strict")
		addedMarkers(report, file, lang)
		if len(report.Violations()) == before {
			note := "no test or assertion lost, no skip added"
			if file.untracked {
				note = "new, no skip added"
			}
			report.Check(file.path, true, note)
		}
	}
	if checked == 0 {
		report.Check("tests", true, "no test file changed since base "+base)
	}
}

// netLoss fails the file when it removes more pattern lines than it adds,
// pointing at the first removed one.
func netLoss(report *protocol.GateReport, file *fileDiff, lang *testLanguage,
	pattern *regexp.Regexp, what, fix string) {
	removed := lang.matching(file.removed, pattern)
	added := lang.matching(file.added, pattern)
	if len(removed) <= len(added) {
		return
	}
	report.Check(fmt.Sprintf("%s:%d", file.path, removed[0].line), false, fmt.Sprintf(
		"net loss of %d %s (removed %d, added %d); %s",
		len(removed)-len(added), what, len(removed), len(added), fix))
}

// addedMarkers fails every added skip/focus marker beyond what the same file
// removes of that kind, so a moved marker or a reworded skip message is not
// a new skip.
func addedMarkers(report *protocol.GateReport, file *fileDiff, lang *testLanguage) {
	removed := map[string]int{}
	for _, hit := range lang.markersIn(file.removed) {
		removed[hit.kind]++
	}
	// One finding per line: `pytestmark = pytest.mark.skip` is two markers
	// but one thing to remove.
	reported := map[int]bool{}
	for _, hit := range lang.markersIn(file.added) {
		if removed[hit.kind] > 0 {
			removed[hit.kind]--
			continue
		}
		if reported[hit.line.line] {
			continue
		}
		reported[hit.line.line] = true
		report.Check(fmt.Sprintf("%s:%d", file.path, hit.line.line), false, fmt.Sprintf(
			"adds a skip/focus marker (%s); remove it and make the test pass instead — "+
				"a skipped or excluded test checks nothing, and a focused one silences the rest of the suite",
			strings.TrimSpace(hit.line.text)))
	}
}

// allowedBy returns the first allow glob matching file, or "". Globs were
// validated at save time, so a match error cannot occur here.
func allowedBy(file string, allow []string) string {
	for _, glob := range allow {
		if ok, _ := path.Match(glob, file); ok {
			return glob
		}
	}
	return ""
}
