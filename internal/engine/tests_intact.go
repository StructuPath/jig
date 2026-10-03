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
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// testPathPattern is factory.yaml's classify-risk `tests` pattern plus
// pytest's test_*.py, so the gate and the risk classifier agree on what a
// test file is.
var testPathPattern = regexp.MustCompile(
	`(_test\.go|_test\.py|\.(test|spec)\.[cm]?[jt]sx?|_spec\.rb)$|(^|/)test_[^/]*\.py$|(^|/)(tests?|__tests__|spec)/`)

// testLanguage is one v1 language's line patterns. A test file in a language
// with none (a fixture under tests/, say) is still checked for deletion.
type testLanguage struct {
	comment *regexp.Regexp // a whole-line comment, ignored by every count
	tests   *regexp.Regexp // declares one test
	asserts *regexp.Regexp // makes one assertion
	skips   *regexp.Regexp // skips or focuses tests
}

var (
	langGo = &testLanguage{
		comment: regexp.MustCompile(`^\s*(//|/\*|\*)`),
		tests:   regexp.MustCompile(`^\s*func\s+Test`),
		asserts: regexp.MustCompile(`\bt\.(Error|Fatal|Fail)|\b(assert|require)\.\w+\s*\(`),
		skips:   regexp.MustCompile(`\b[tb]\.Skip`),
	}
	langJS = &testLanguage{
		comment: regexp.MustCompile(`^\s*(//|/\*|\*)`),
		tests:   regexp.MustCompile(`\b(it|test)\s*\(`),
		asserts: regexp.MustCompile(`\bexpect\s*\(|\bassert(\.\w+)?\s*\(`),
		skips:   regexp.MustCompile(`\.(skip|only)\s*\(|\b(xit|xdescribe|xtest)\s*\(`),
	}
	langPython = &testLanguage{
		comment: regexp.MustCompile(`^\s*#`),
		tests:   regexp.MustCompile(`^\s*(async\s+)?def\s+test`),
		asserts: regexp.MustCompile(`^\s*assert\b|\bself\.assert\w*\s*\(`),
		skips: regexp.MustCompile(`@pytest\.mark\.(skip|skipif|xfail)\b|@unittest\.(skip\w*|expectedFailure)\b|` +
			`\bpytest\.(skip|xfail)\s*\(|\bself\.skipTest\s*\(`),
	}
	langRuby = &testLanguage{
		comment: regexp.MustCompile(`^\s*#`),
		tests:   regexp.MustCompile(`^\s*(it|specify)\b`),
		asserts: regexp.MustCompile(`\bexpect\s*[({]|\bshould(_not)?\b`),
		skips:   regexp.MustCompile(`^\s*(skip|pending|xit|xspecify|xdescribe|xcontext|fit|fdescribe|fcontext)\b`),
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
	path           string
	deleted        bool
	added, removed []diffLine
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseUnifiedDiff reads `git diff -U0` output. Header lines are only read
// before a file's first hunk, so a removed line that itself starts with
// "--- " is never mistaken for one; "diff --git" can only be a header,
// because every hunk line starts with a marker.
func parseUnifiedDiff(output string) []*fileDiff {
	var files []*fileDiff
	var current *fileDiff
	inHunk := false
	newLine, anchor := 0, 0
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
			anchor = newLine
			// A pure deletion's new start is the line BEFORE the gap.
			if match[2] == "0" {
				anchor++
			}
			continue
		}
		if !inHunk {
			switch {
			case strings.HasPrefix(line, "deleted file mode"):
				current.deleted = true
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
			current.removed = append(current.removed, diffLine{anchor, line[1:]})
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
// comments: commenting a test out is a removal, and a commented-out
// t.Skip is no skip.
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

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

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
	// diff.mnemonicPrefix cannot change the header shape this parses, and
	// external diff drivers and textconv are off: the worktree's own config
	// must not decide what this gate reads.
	output, err := runGit(gc.ctx, gc.worktree, "diff", "--no-renames", "--no-ext-diff", "--no-textconv",
		"--no-color", "--unified=0", "--src-prefix=a/", "--dst-prefix=b/", gc.baseSHA, "--")
	if err != nil {
		report.Check("git diff", false, err.Error())
		return report
	}
	base := shortSHA(gc.baseSHA)
	checked := 0
	for _, file := range parseUnifiedDiff(output) {
		if !testPathPattern.MatchString(file.path) {
			continue
		}
		checked++
		if glob := allowedBy(file.path, gc.allow); glob != "" {
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
		lang := testLanguageOf(file.path)
		if lang == nil {
			report.Check(file.path, true, "changed, not deleted")
			continue
		}
		before := len(report.Violations())
		netLoss(&report, file, lang, lang.tests, "test function(s)",
			"restore the removed tests, or rewrite each as an equivalent test; deleting a failing test does not fix the code")
		netLoss(&report, file, lang, lang.asserts, "assertion(s)",
			"restore the removed assertions or replace each with one at least as strict")
		addedSkips(&report, file, lang)
		if len(report.Violations()) == before {
			report.Check(file.path, true, "no test or assertion lost, no skip added")
		}
	}
	if checked == 0 {
		report.Check("tests", true, "no test file changed since base "+base)
	}
	return report
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

// addedSkips fails every added skip/focus marker. A marker whose exact text
// the same file also removes is a moved or re-indented line, not a new skip.
func addedSkips(report *protocol.GateReport, file *fileDiff, lang *testLanguage) {
	removed := map[string]int{}
	for _, line := range lang.matching(file.removed, lang.skips) {
		removed[strings.TrimSpace(line.text)]++
	}
	for _, line := range lang.matching(file.added, lang.skips) {
		text := strings.TrimSpace(line.text)
		if removed[text] > 0 {
			removed[text]--
			continue
		}
		report.Check(fmt.Sprintf("%s:%d", file.path, line.line), false, fmt.Sprintf(
			"adds a skip/focus marker (%s); remove it and make the test pass instead — "+
				"a skipped test checks nothing, and a focused one silences the rest of the suite", text))
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
