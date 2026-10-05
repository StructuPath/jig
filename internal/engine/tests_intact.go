// tests_intact.go — the tests_intact gate (R9). A repair loop that feeds red
// tests or red CI back to a builder invites the cheapest "fix": delete the
// failing test, drop its assertion, or mark it skipped (the CI-repair plan's
// "weakening the check" risk). The risk classifier only holds such work for a
// person after the fact; this gate refutes it at the phase that wrote it, so
// the builder is told in the same session to put the check back.
//
// It is a tripwire against careless or opportunistic tampering, not an
// adversarially complete analysis. Each changed test file is compared
// whole, base against worktree: both sides are normalized (comments
// stripped by a small string-aware lexer, string contents blanked,
// whitespace collapsed so tokens split across lines rejoin), then tests,
// assertions, and every skip/focus marker kind are counted on each side.
// Fewer tests or assertions, or more of any marker, fails. The hunk diff
// only supplies a best-effort line number for the finding. Whatever the
// gate cannot inspect — a binary or oversized file, a hidden index flag, an
// embedded repository — fails closed rather than passing unread.
package engine

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/StructuPath/jig/internal/protocol"
)

// testPathPattern is factory.yaml's classify-risk `tests` pattern plus
// pytest's test_*.py and conftest.py (where collect_ignore and collection
// hooks hide whole files), so the gate and the risk classifier agree on
// what a test file is.
var testPathPattern = regexp.MustCompile(
	`(_test\.go|_test\.py|\.(test|spec)\.[cm]?[jt]sx?|_spec\.rb)$|(^|/)(test_[^/]*|conftest)\.py$|(^|/)(tests?|__tests__|spec)/`)

// maxTestFileBytes is the largest test file the gate reads. A larger one is
// a finding, never a silent truncation: the part left unread is exactly
// where a skip would go.
const maxTestFileBytes = 4 << 20

// lexStyle is what a language's comment-and-string lexer recognizes.
type lexStyle struct {
	slashComments   bool // `//` and `/* */`
	hashComments    bool // `#`
	ruby            bool // `=begin`/`=end`, heredocs, `%r{…}` and other % literals
	tripleQuotes    bool // Python `"""` and `'''`
	backticks       bool // a backtick string: Go raw, JS template
	backtickEscapes bool // JS templates honor `\`; Go raw strings do not
	goDirectives    bool // keep `//go:build` and `// +build` lines as tokens
	// regexLiterals: a `/` in expression position opens a regex, so a `/*`
	// or `#` inside one is not a comment (JS, Ruby).
	regexLiterals bool
}

// markerPattern finds one thing that stops tests from running: a skip, a
// focus that silences the rest, a constraint or collection rule that drops a
// file, an exit that ends the run early. A named `kind` group, when present,
// identifies the marker; otherwise the whole match does. Patterns match the
// normalized form: no comments, strings as "", whitespace only between two
// word characters.
type markerPattern struct{ re *regexp.Regexp }

// lossCount is a count that must not fall: tests, assertions, m.Run calls.
type lossCount struct {
	re   *regexp.Regexp
	what string // "test function(s)"
	fix  string
}

// testLanguage is one v1 language. A test file in a language with none (a
// fixture under tests/, say) is still checked for deletion and size.
type testLanguage struct {
	lex     lexStyle
	losses  []lossCount
	markers []markerPattern
	// optionMarkers finds a test call's options object (group `opts`):
	// each marker key set to anything but a falsy literal counts as that
	// kind, so `test.skip(…)` → `test(…, {skip: true}, …)` is no change.
	optionMarkers *regexp.Regexp
}

const (
	fixTests      = "restore the removed tests, or rewrite each as an equivalent test; deleting a failing test does not fix the code"
	fixAssertions = "restore the removed assertions or replace each with one at least as strict"
)

var (
	langGo = &testLanguage{
		lex: lexStyle{slashComments: true, backticks: true, goDirectives: true},
		losses: []lossCount{
			{regexp.MustCompile(`\bfunc Test\w*\(`), "test function(s)", fixTests},
			{regexp.MustCompile(`\bt\.(Error|Fatal|Fail)|\b(assert|require)\.\w+\(`), "assertion(s)", fixAssertions},
			// os.Exit(m.Run()) → os.Exit(0) keeps TestMain and os.Exit
			// and runs nothing.
			{regexp.MustCompile(`\bm\.Run\(`), "m.Run call(s)",
				"TestMain must still call m.Run() and exit with its result, or the suite never runs"},
		},
		markers: []markerPattern{
			{regexp.MustCompile(`\b[tb]\.Skip\w*`)},
			// A TestMain owns the run: it can return without m.Run().
			{regexp.MustCompile(`\bfunc TestMain\(`)},
			{regexp.MustCompile(`\bos\.Exit\(`)},
			// The whole constraint is the kind, so changing one counts as
			// adding one: `//go:build linux` → `//go:build ignore`.
			{regexp.MustCompile("\x00build:[^\x00]*\x00")},
		},
	}
	// langGoTest is a _test.go file, where `go test` also runs examples and
	// fuzz targets. A named func with no receiver is always top-level.
	langGoTest = &testLanguage{
		lex: langGo.lex,
		losses: append([]lossCount{{
			regexp.MustCompile(`\bfunc (Test\w*\(|Example\w*\(\)|Fuzz\w*\(\w+\*testing\.F\))`),
			"test function(s)", fixTests}}, langGo.losses[1:]...),
		markers: langGo.markers,
	}
	langJS = &testLanguage{
		lex: lexStyle{slashComments: true, backticks: true, backtickEscapes: true, regexLiterals: true},
		losses: []lossCount{
			// Any chain counts as the test it declares, so test(…) →
			// test.each(…)(…) is no loss; a .only/.skip is a marker below.
			{regexp.MustCompile(`(^|[^.\w$])(it|test)(\.\w+)*(\(|"")`), "test function(s)", fixTests},
			{regexp.MustCompile(`\bexpect\(|\bassert(\.\w+)?\(`), "assertion(s)", fixAssertions},
		},
		markers: []markerPattern{
			// vitest's skipIf/runIf/fails included: each turns a test off
			// or inverts it.
			{regexp.MustCompile(`\b(describe|context|suite|it|test|specify|bench)(\.\w+)*?\.(?P<kind>only|skip|todo|skipIf|runIf|fails)\b`)},
			{regexp.MustCompile(`(^|[^.\w$])(?P<kind>xit|xdescribe|xtest|xcontext|fit|fdescribe)[.(]`)},
		},
		// vitest's options object, second argument: test("…", {skip: true}, fn).
		optionMarkers: regexp.MustCompile(
			`(^|[^.\w$])(describe|context|suite|it|test|specify|bench)(\.\w+)*\([^(){},]*,(?P<opts>\{[^{}]*\})`),
	}
	langPython = &testLanguage{
		lex: lexStyle{hashComments: true, tripleQuotes: true},
		losses: []lossCount{
			{regexp.MustCompile(`\bdef test\w*\(`), "test function(s)", fixTests},
			{regexp.MustCompile(`(^|[^.\w])assert\b|\bself\.assert\w*\(`), "assertion(s)", fixAssertions},
		},
		markers: []markerPattern{
			// `mark.skip`, not `@pytest.mark.skip`, so parenthesized,
			// aliased, and pytest.param(marks=…) forms all count.
			{regexp.MustCompile(`\bmark\.(skip|skipif|xfail)\b`)},
			{regexp.MustCompile(`\bunittest\.(skip\w*|expectedFailure)\b`)},
			{regexp.MustCompile(`\bpytest\.(skip|xfail|importorskip)\(|\bself\.skipTest\(`)},
			// unittest's decorators imported bare: `from unittest import skip`.
			{regexp.MustCompile(`@(?P<kind>skipIf|skipUnless|skip|expectedFailure)\b`)},
			{regexp.MustCompile(`\braise (unittest\.)?(?P<kind>SkipTest)\b`)},
			{regexp.MustCompile(`(^|[^.\w])(?P<kind>pytestmark)(:[^=]+)?\+?=`)},
			{regexp.MustCompile(`(^|[^.\w])(?P<kind>__test__=False)\b`)},
			// An alias puts mark.skip out of a pattern's reach: flag the
			// alias itself.
			{regexp.MustCompile(`\b(?P<kind>(mark|skip|skipIf|skipUnless|expectedFailure|SkipTest) as)\b`)},
			{regexp.MustCompile(`=(?P<kind>pytest\.mark)([^.\w]|$)`)},
			{regexp.MustCompile(`\bdef (?P<kind>pytest_(ignore_collect|collection_modifyitems))\(`)},
			{regexp.MustCompile(`(^|[^.\w])(?P<kind>collect_ignore(_glob)?)(:[^=]+)?(\+?=|\.(append|extend|insert)\()`)},
		},
	}
	langRuby = &testLanguage{
		lex: lexStyle{hashComments: true, ruby: true, regexLiterals: true},
		losses: []lossCount{
			// `it` followed by a description, a paren, a block — not
			// Ruby 3.4's implicit block parameter.
			{regexp.MustCompile(`(^|[^.\w])(it|specify|example)(\(|""|\{| ?do\b)`), "test function(s)", fixTests},
			{regexp.MustCompile(`\bexpect[({]|\bshould(_not)?\b`), "assertion(s)", fixAssertions},
		},
		markers: []markerPattern{
			// As a call (`skip`, `xit "…"`) or as metadata (`skip: true`,
			// `:focus`) — the normalized form reads both the same way.
			{regexp.MustCompile(`(^|[^.\w])(?P<kind>skip|pending|xit|xspecify|xexample|xdescribe|xcontext|fit|fdescribe|fcontext|focus)\b`)},
		},
	}
	// optionKey is one marker key in a normalized options object.
	optionKey   = regexp.MustCompile(`(?:^|[{,])(?P<kind>skip|only|fails|todo):(?P<value>[^,}]*)`)
	jsExtension = regexp.MustCompile(`\.[cm]?[jt]sx?$`)
	goDirective = regexp.MustCompile(`^//\s*(go:build|\+build)\b`)
)

func testLanguageOf(file string) *testLanguage {
	switch {
	case strings.HasSuffix(file, "_test.go"):
		return langGoTest
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

// stripCode drops comments and blanks string, regex, and heredoc contents to
// `""`, so neither a commented-out test nor a marker spelled inside a
// literal counts. A comment becomes a space, so `a/*x*/b` stays two tokens.
// It is a lexer, not a parser. The second result is a problem when a
// comment, string, or literal is still open at end of file: everything after
// an unclosed opener went unread, and the gate does not vouch for it.
func stripCode(src string, style lexStyle) (string, string) {
	src = strings.TrimPrefix(src, string(rune(0xFEFF)))
	out := make([]byte, 0, len(src))
	n := len(src)
	var heredocs []string // Ruby heredoc terminators opened on this line
	for i := 0; i < n; {
		c := src[i]
		lineStart := i == 0 || src[i-1] == '\n'
		switch {
		case style.slashComments && strings.HasPrefix(src[i:], "//"):
			end := lineEnd(src, i)
			if style.goDirectives && goDirective.MatchString(src[i:end]) {
				out = append(out, " \x00build:"+strings.Join(strings.Fields(src[i+2:end]), " ")+"\x00 "...)
			}
			out = append(out, ' ')
			i = end
		case style.slashComments && strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return "", "a /* block comment"
			}
			out = append(out, ' ')
			i += 2 + end + 2
		case style.hashComments && c == '#':
			out = append(out, ' ')
			i = lineEnd(src, i)
		case style.ruby && lineStart && strings.HasPrefix(src[i:], "=begin"):
			end := strings.Index(src[i:], "\n=end")
			if end < 0 {
				return "", "an =begin block"
			}
			out = append(out, ' ')
			i = lineEnd(src, i+end+1)
		case style.tripleQuotes && (strings.HasPrefix(src[i:], `"""`) || strings.HasPrefix(src[i:], `'''`)):
			end := strings.Index(src[i+3:], src[i:i+3])
			if end < 0 {
				return "", "a triple-quoted string"
			}
			out = append(out, `""`...)
			i += 3 + end + 3
		case style.regexLiterals && c == '/' && regexPosition(out, src, i, style.ruby):
			if end, ok := scanRegex(src, i); ok {
				out = append(out, `""`...)
				i = end
				continue
			}
			out = append(out, c) // no closing `/` on the line: division after all
			i++
		case style.ruby && c == '%' && i+2 < n && strings.IndexByte("qQwWiIrsx", src[i+1]) >= 0 &&
			isLiteralDelimiter(src[i+2]):
			end, ok := scanPercentLiteral(src, i+2)
			if !ok {
				return "", "a %" + string(src[i+1]) + " literal"
			}
			out = append(out, `""`...)
			i = end
		case style.ruby && strings.HasPrefix(src[i:], "<<") && rubyHeredocStart(src[i:]) != nil:
			match := rubyHeredocStart(src[i:])
			heredocs = append(heredocs, match[3])
			out = append(out, `""`...)
			i += len(match[0])
		case c == '\n' && len(heredocs) > 0:
			// The rest of the opening line has been read; the bodies follow,
			// in order, and none of them is code.
			out = append(out, '\n')
			i++
			for _, terminator := range heredocs {
				var ok bool
				if i, ok = skipHeredoc(src, i, terminator); !ok {
					return "", "a heredoc"
				}
			}
			heredocs = nil
		case c == '"' || c == '\'' || (c == '`' && style.backticks):
			escapes := c != '`' || style.backtickEscapes
			j, closed := i+1, false
			for j < n {
				if escapes && src[j] == '\\' {
					j += 2
					continue
				}
				if src[j] == c {
					j++
					closed = true
					break
				}
				if src[j] == '\n' && c != '`' {
					break // a one-line string ends at the line, closed or not
				}
				j++
			}
			if !closed && j >= n {
				return "", "a string or template literal"
			}
			out = append(out, `""`...)
			i = min(j, n)
		default:
			out = append(out, c)
			i++
		}
	}
	if len(heredocs) > 0 {
		return "", "a heredoc"
	}
	return string(out), ""
}

// regexKeywords end an expression's left side: a `/` after one opens a regex.
var regexKeywords = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true, "of": true, "new": true,
	"delete": true, "void": true, "throw": true, "case": true, "do": true, "else": true,
	"yield": true, "await": true, "when": true, "if": true, "unless": true, "and": true,
	"or": true, "not": true, "while": true, "until": true, "elsif": true, "then": true,
}

// regexPosition reports whether a `/` at src[i] is in expression position:
// at a line start, after an operator or opener, or after a keyword — where
// it cannot be division. In Ruby, `match /re/` (a name, a space, then no
// space) is a regex argument, which is how Ruby itself reads it.
func regexPosition(out []byte, src string, i int, ruby bool) bool {
	k := len(out) - 1
	spaced := false
	for k >= 0 && (out[k] == ' ' || out[k] == '\t' || out[k] == '\r') {
		k--
		spaced = true
	}
	if k < 0 || out[k] == '\n' {
		return true
	}
	prev := out[k]
	// `=>` returns an expression; a bare `>` is a comparison, whose right
	// side is never directly a `/`, so it stays out.
	if strings.IndexByte("(,=:[!&|?{};~", prev) >= 0 || prev == '>' && k > 0 && out[k-1] == '=' {
		return true
	}
	if !isWordByte(prev) {
		return false
	}
	start := k
	for start > 0 && isWordByte(out[start-1]) {
		start--
	}
	if regexKeywords[string(out[start:k+1])] {
		return true
	}
	return ruby && spaced && i+1 < len(src) && src[i+1] != ' ' && src[i+1] != '='
}

// scanRegex finds the end of a regex literal opening at src[i]: the first
// unescaped `/` outside a `[...]` class. A newline first means it was never
// a regex.
func scanRegex(src string, i int) (int, bool) {
	inClass := false
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				return j + 1, true
			}
		case '\n':
			return 0, false
		}
	}
	return 0, false
}

func isLiteralDelimiter(c byte) bool {
	return c > ' ' && c < 0x7f && !isWordByte(c)
}

// scanPercentLiteral finds the end of a Ruby %-literal whose delimiter is at
// src[open]; bracket delimiters nest.
func scanPercentLiteral(src string, open int) (int, bool) {
	closer := src[open]
	switch closer {
	case '(':
		closer = ')'
	case '[':
		closer = ']'
	case '{':
		closer = '}'
	case '<':
		closer = '>'
	}
	depth := 1
	for j := open + 1; j < len(src); j++ {
		switch {
		case src[j] == '\\':
			j++
		case closer != src[open] && src[j] == src[open]:
			depth++
		case src[j] == closer:
			depth--
			if depth == 0 {
				return j + 1, true
			}
		}
	}
	return 0, false
}

var rubyHeredocPattern = regexp.MustCompile("^<<([~-]?)([\"'`]?)([A-Za-z_]\\w*)([\"'`]?)")

// rubyHeredocStart matches a heredoc opener. A bare `<<NAME` needs an
// upper-case name and `<<~`/`<<-` or quotes otherwise, so `items<<item` stays
// an append.
func rubyHeredocStart(s string) []string {
	match := rubyHeredocPattern.FindStringSubmatch(s)
	if match == nil || match[2] != match[4] {
		return nil
	}
	if match[1] == "" && match[2] == "" && !unicode.IsUpper(rune(match[3][0])) {
		return nil
	}
	return match
}

// skipHeredoc skips a heredoc body from src[i] through its terminator line.
func skipHeredoc(src string, i int, terminator string) (int, bool) {
	for i < len(src) {
		end := lineEnd(src, i)
		if strings.TrimSpace(src[i:end]) == terminator {
			return min(end+1, len(src)), true
		}
		i = end + 1
	}
	return 0, false
}

func lineEnd(src string, from int) int {
	if end := strings.IndexByte(src[from:], '\n'); end >= 0 {
		return from + end
	}
	return len(src)
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))
}

// collapseSpace removes all whitespace, newlines included, except one space
// between two word characters: `t.\n\tSkip` → `t.Skip`, `test\n  .only` →
// `test.only`, while `func TestMain` keeps the space that separates them.
func collapseSpace(s string) string {
	out := make([]byte, 0, len(s))
	pending := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' {
			pending = true
			continue
		}
		if pending && len(out) > 0 && isWordByte(out[len(out)-1]) && isWordByte(c) {
			out = append(out, ' ')
		}
		pending = false
		out = append(out, c)
	}
	return string(out)
}

// normalize is a whole file's normalized form, or the construct left open at
// end of file.
func (l *testLanguage) normalize(src string) (string, string) {
	stripped, problem := stripCode(src, l.lex)
	return collapseSpace(stripped), problem
}

// normalizeLine normalizes one line out of context, for line numbers only;
// a construct it leaves open is expected there and costs nothing.
func (l *testLanguage) normalizeLine(src string) string {
	normalized, _ := l.normalize(src)
	return normalized
}

// uninspectableText names why content cannot be read as source text: a NUL
// byte, or a UTF-16/UTF-32 byte-order mark (those encodings put NULs
// between the ASCII a pattern looks for, so nothing would match).
func uninspectableText(content string) string {
	switch {
	case strings.HasPrefix(content, "\xFF\xFE"), strings.HasPrefix(content, "\xFE\xFF"),
		strings.HasPrefix(content, "\x00\x00\xFE\xFF"):
		return "is UTF-16 or UTF-32 encoded"
	case strings.IndexByte(content, 0) >= 0:
		return "contains a NUL byte"
	}
	return ""
}

// markerKinds counts each marker kind in normalized text.
func (l *testLanguage) markerKinds(normalized string) map[string]int {
	kinds := map[string]int{}
	for _, marker := range l.markers {
		kindIndex := marker.re.SubexpIndex("kind")
		for _, match := range marker.re.FindAllStringSubmatch(normalized, -1) {
			kind := match[0]
			if kindIndex >= 0 {
				kind = match[kindIndex]
			}
			kinds[kind]++
		}
	}
	if l.optionMarkers != nil {
		opts := l.optionMarkers.SubexpIndex("opts")
		for _, match := range l.optionMarkers.FindAllStringSubmatch(normalized, -1) {
			for _, option := range optionKey.FindAllStringSubmatch(match[opts], -1) {
				switch option[2] {
				case "false", "0", "null", "undefined":
				default:
					kinds[option[1]]++
				}
			}
		}
	}
	return kinds
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
	created bool // not in the base: its base content is empty
	// binary: git reported the file as binary, so it has no hunks to read.
	binary         bool
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
			case strings.HasPrefix(line, "new file mode"):
				current.created = true
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

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// gateTestsIntact: no test file changed since the pinned base may lose a
// test, an assertion, or an m.Run call, or gain a skip/focus marker, and
// none may be deleted — unless the definition's allow list names it. Counts
// are whole-file, so a rename inside the file, an assertion replaced with a
// stronger one, or a reworded skip passes; a renamed FILE is a deletion,
// since --no-renames keeps the old path visible as gone.
func gateTestsIntact(gc gateContext) protocol.GateReport {
	var report protocol.GateReport
	if gc.baseSHA == "" {
		report.Check("base commit", false, "the attempt has no pinned base commit to diff against")
		return report
	}
	j := &intactJudge{gc: gc, report: &report, base: shortSHA(gc.baseSHA)}
	if !j.judgeTracked() || !j.judgeUntracked() || !j.judgeIndexFlags() {
		return report
	}
	if j.checked == 0 {
		report.Check("tests", true, "no test file changed since base "+j.base)
	}
	return report
}

type intactJudge struct {
	gc      gateContext
	report  *protocol.GateReport
	base    string
	checked int
}

// git runs one hardened git command; a failure is a failed check, and the
// gate stops, since what it could not list it cannot vouch for.
func (j *intactJudge) git(item string, args ...string) (string, bool) {
	output, err := runGit(j.gc.ctx, j.gc.worktree, args...)
	if err != nil {
		j.report.Check(item, false, err.Error())
		return "", false
	}
	return output, true
}

// exempt counts a test path and reports whether allow covers it.
func (j *intactJudge) exempt(file string) bool {
	j.checked++
	if glob := allowedBy(file, j.gc.allow); glob != "" {
		j.report.Check(file, true, fmt.Sprintf("exempt: allowed by %q", glob))
		return true
	}
	return false
}

// judgeTracked compares every tracked test file the diff shows as changed.
func (j *intactJudge) judgeTracked() bool {
	// The diff only finds changed files and line numbers; the verdict comes
	// from whole-file contents. Prefixes are pinned against diff.noprefix
	// and diff.mnemonicPrefix, external drivers and textconv are off,
	// --text overrides a `-diff` attribute, and no inter-hunk context keeps
	// line numbers exact: the worktree's config must not shape what this
	// reads.
	output, ok := j.git("git diff", "diff", "--no-renames", "--no-ext-diff", "--no-textconv",
		"--text", "--no-color", "--unified=0", "--inter-hunk-context=0",
		"--src-prefix=a/", "--dst-prefix=b/", j.gc.baseSHA, "--")
	if !ok {
		return false
	}
	for _, file := range parseUnifiedDiff(output) {
		j.judgeDiffFile(file)
	}
	return true
}

// judgeDiffFile judges one file the diff shows as changed.
func (j *intactJudge) judgeDiffFile(file *fileDiff) {
	if !testPathPattern.MatchString(file.path) || j.exempt(file.path) {
		return
	}
	switch {
	case file.deleted:
		j.report.Check(file.path, false, fmt.Sprintf(
			"test file deleted since base %s; restore it (git checkout %s -- %s). "+
				"Removing a test is only allowed when the definition's tests_intact allow list names it",
			j.base, j.base, file.path))
		return
	case file.binary:
		// Unreachable with --text, unless git changes; a file the gate
		// cannot read is never one it vouches for.
		j.report.Check(file.path, false,
			"git reports this test file as binary, so its changes cannot be inspected; "+
				"restore it as text or name it in the tests_intact allow list")
		return
	}
	before := ""
	if !file.created {
		var problem string
		before, problem = j.baseContent(file.path)
		if problem != "" {
			j.report.Check(file.path, false, problem)
			return
		}
	}
	after, problem := readTestFile(j.gc.worktree, file.path)
	if problem != "" {
		j.report.Check(file.path, false, problem)
		return
	}
	j.compare(file.path, before, after, file)
}

// baseContent reads a file at the pinned base, refusing one too large to
// read whole.
func (j *intactJudge) baseContent(file string) (string, string) {
	object := j.gc.baseSHA + ":" + file
	size, err := runGit(j.gc.ctx, j.gc.worktree, "cat-file", "-s", object)
	if err != nil {
		return "", "base content could not be read: " + err.Error()
	}
	if n, err := strconv.Atoi(strings.TrimSpace(size)); err != nil || n > maxTestFileBytes {
		return "", tooLarge("base version", strings.TrimSpace(size))
	}
	content, err := runGit(j.gc.ctx, j.gc.worktree, "show", "--no-textconv", object)
	if err != nil {
		return "", "base content could not be read: " + err.Error()
	}
	return content, ""
}

func tooLarge(which, size string) string {
	return fmt.Sprintf("too large to inspect: the %s is %s bytes, over the %d-byte limit; "+
		"split the file or name it in the tests_intact allow list", which, size, maxTestFileBytes)
}

// readTestFile reads a worktree file whole. Anything but a regular file
// fails: a symlink could point outside the worktree, and the gate neither
// follows it nor vouches for what it cannot read.
func readTestFile(worktree, name string) (string, string) {
	full, ok := artifactPath(worktree, filepath.FromSlash(name))
	if !ok {
		return "", "path is not inside the worktree"
	}
	info, err := os.Lstat(full)
	if err != nil {
		return "", "could not be read: " + err.Error()
	}
	if !info.Mode().IsRegular() {
		return "", "is not a regular file, so it cannot be inspected; restore it as one"
	}
	if info.Size() > maxTestFileBytes {
		return "", tooLarge("worktree version", strconv.FormatInt(info.Size(), 10))
	}
	content, err := os.ReadFile(full)
	if err != nil {
		return "", "could not be read: " + err.Error()
	}
	if len(content) > maxTestFileBytes {
		return "", tooLarge("worktree version", strconv.Itoa(len(content)))
	}
	return string(content), ""
}

// runnerSkips is a dependency tree the test runners themselves never
// collect from: jest ignores node_modules, `go test ./...` skips vendor,
// pytest skips virtualenvs. Ignored files there are installed packages, not
// the agent's tests, and their own skips are not this change's.
func runnerSkips(file string) bool {
	segments := strings.Split(file, "/")
	for _, segment := range segments[:len(segments)-1] {
		switch {
		case segment == "node_modules":
			return true
		case segment == "vendor" && strings.HasSuffix(file, ".go"):
			return true
		case (segment == "venv" || segment == ".venv" || segment == "site-packages") &&
			strings.HasSuffix(file, ".py"):
			return true
		}
	}
	return false
}

// judgeUntracked reads every untracked test file, gitignored ones included —
// a .gitignore entry hides a file from git, not from the runner — as all
// added. Embedded repositories, which git lists as one opaque directory,
// fail when they hold test files.
func (j *intactJudge) judgeUntracked() bool {
	listed, ok := j.git("git ls-files", "ls-files", "-z", "--others")
	if !ok {
		return false
	}
	for _, name := range strings.Split(listed, "\x00") {
		switch {
		case name == "":
		case strings.HasSuffix(name, "/"):
			j.judgeEmbedded(strings.TrimSuffix(name, "/"))
		case !testPathPattern.MatchString(name) || runnerSkips(name) || j.exempt(name):
		default:
			content, problem := readTestFile(j.gc.worktree, name)
			if problem != "" {
				j.report.Check(name, false, "untracked test file "+problem)
				continue
			}
			file := &fileDiff{path: name, created: true}
			for i, text := range strings.Split(content, "\n") {
				file.added = append(file.added, diffLine{i + 1, text})
			}
			j.compare(name, "", content, file)
		}
	}
	return true
}

// judgeEmbedded fails an embedded git repository holding test files: git
// shows it as one entry and never its contents, so changes in it are
// invisible to the diff.
func (j *intactJudge) judgeEmbedded(dir string) {
	root, ok := artifactPath(j.gc.worktree, filepath.FromSlash(dir))
	if !ok {
		return
	}
	var found []string
	_ = filepath.WalkDir(root, func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(j.gc.worktree, full)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if testPathPattern.MatchString(rel) && !runnerSkips(rel) && allowedBy(rel, j.gc.allow) == "" {
			found = append(found, rel)
		}
		return nil
	})
	if len(found) == 0 {
		return
	}
	j.checked++
	j.report.Check(dir+"/", false, fmt.Sprintf(
		"embedded git repository holds %d test file(s) (%s) that git cannot show this gate; "+
			"remove its .git directory or move the tests into the repository", len(found), found[0]))
}

// judgeIndexFlags fails a tracked test file marked assume-unchanged or
// skip-worktree: git diff then reports it unchanged whatever its contents.
func (j *intactJudge) judgeIndexFlags() bool {
	listed, ok := j.git("git ls-files -v", "ls-files", "-v", "-z")
	if !ok {
		return false
	}
	for _, entry := range strings.Split(listed, "\x00") {
		if len(entry) < 3 {
			continue
		}
		tag, name := entry[0], entry[2:]
		if !(tag == 'S' || unicode.IsLower(rune(tag))) || !testPathPattern.MatchString(name) || j.exempt(name) {
			continue
		}
		j.report.Check(name, false, "index flag assume-unchanged or skip-worktree hides this test file's changes "+
			"from git; clear it (git update-index --no-assume-unchanged --no-skip-worktree "+name+")")
	}
	return true
}

// compare judges one test file, base content against current. file supplies
// the raw changed lines, used only to put a line number on a finding.
func (j *intactJudge) compare(name, before, after string, file *fileDiff) {
	lang := testLanguageOf(name)
	if lang == nil {
		j.report.Check(name, true, "changed, not deleted")
		return
	}
	failed := len(j.report.Violations())
	// Only source files are read as text: a binary fixture under tests/ is
	// judged on deletion alone, above.
	sides := []struct{ which, content string }{{"base", before}, {"worktree", after}}
	for _, side := range sides {
		if problem := uninspectableText(side.content); problem != "" {
			j.report.Check(name, false, fmt.Sprintf(
				"uninspectable: the %s version %s, so it cannot be read as source; "+
					"save it as UTF-8 text or name it in the tests_intact allow list", side.which, problem))
			return
		}
	}
	old, oldOpen := lang.normalize(before)
	now, nowOpen := lang.normalize(after)
	for _, open := range []struct{ which, construct string }{{"base", oldOpen}, {"worktree", nowOpen}} {
		if open.construct != "" {
			j.report.Check(name, false, fmt.Sprintf(
				"unterminated: in the %s version %s is still open at end of file, so the rest of the "+
					"file cannot be read; close it or name the file in the tests_intact allow list",
				open.which, open.construct))
			return
		}
	}
	for _, loss := range lang.losses {
		was, is := len(loss.re.FindAllStringIndex(old, -1)), len(loss.re.FindAllStringIndex(now, -1))
		if is >= was {
			continue
		}
		j.report.Check(findingItem(name, lineOf(lang, file.removed, loss.re)), false, fmt.Sprintf(
			"net loss of %d %s (%d at base %s, %d now); %s", was-is, loss.what, was, j.base, is, loss.fix))
	}
	oldKinds, nowKinds := lang.markerKinds(old), lang.markerKinds(now)
	// One finding per line: `pytestmark = pytest.mark.skip` gains two kinds
	// but is one thing to remove.
	reported := map[int]bool{}
	for _, kind := range sortedKeys(nowKinds) {
		gained := nowKinds[kind] - oldKinds[kind]
		if gained <= 0 {
			continue
		}
		lines := markerLines(lang, file, kind)
		for k := 0; k < gained; k++ {
			shown, line := kind, diffLine{}
			if k < len(lines) {
				line = lines[k]
				shown = strings.TrimSpace(line.text)
			}
			if line.line > 0 && reported[line.line] {
				continue
			}
			reported[line.line] = true
			j.report.Check(findingItem(name, line.line), false, fmt.Sprintf(
				"adds a skip/focus marker (%s); remove it and make the test pass instead — "+
					"a skipped or excluded test checks nothing, and a focused one silences the rest of the suite",
				shown))
		}
	}
	if len(j.report.Violations()) == failed {
		j.report.Check(name, true, "no test or assertion lost, no skip added")
	}
}

func findingItem(name string, line int) string {
	if line > 0 {
		return fmt.Sprintf("%s:%d", name, line)
	}
	return name
}

// lineOf is the first line whose own normalized text matches re, or 0. One
// line out of context normalizes imperfectly; that only costs the number.
func lineOf(lang *testLanguage, lines []diffLine, re *regexp.Regexp) int {
	for _, line := range lines {
		if re.MatchString(lang.normalizeLine(line.text)) {
			return line.line
		}
	}
	return 0
}

// markerLines lists added lines carrying a marker of kind, preferring those
// whose text the file did not also remove (a moved line is not the gain).
func markerLines(lang *testLanguage, file *fileDiff, kind string) []diffLine {
	removed := map[string]int{}
	for _, line := range file.removed {
		if lang.markerKinds(lang.normalizeLine(line.text))[kind] > 0 {
			removed[strings.TrimSpace(line.text)]++
		}
	}
	var fresh, moved []diffLine
	for _, line := range file.added {
		if lang.markerKinds(lang.normalizeLine(line.text))[kind] == 0 {
			continue
		}
		text := strings.TrimSpace(line.text)
		if removed[text] > 0 {
			removed[text]--
			moved = append(moved, line)
			continue
		}
		fresh = append(fresh, line)
	}
	return append(fresh, moved...)
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
