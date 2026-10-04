package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/StructuPath/jig/internal/protocol"
)

const goTests = `package a

import "testing"

func TestOne(t *testing.T) {
	if 1 != 1 {
		t.Fatal("one")
	}
}

func TestTwo(t *testing.T) {
	if 2 != 2 {
		t.Errorf("two")
	}
}
`

const jsTests = `describe("a", () => {
  it("one", () => {
    expect(1).toBe(1);
  });
  test("two", () => {
    assert.equal(2, 2);
  });
});
test.each([1, 2])("each %i", (n) => {
  expect(n).toBe(n);
});
`

const goSkipped = `package a

import "testing"

func TestSkipped(t *testing.T) {
	t.Skip("old reason")
}
`

// jsRegexTests holds regex literals whose `/*` would open a comment to a
// lexer without a regex state, blinding the rest of the file on both sides.
const jsRegexTests = `const re = /^\/*/;
const cls = /[/*]/;
it("matches", () => {
  expect(re.test("x")).toBe(true);
  expect(cls.test("*")).toBe(true);
});
`

// rbRegexSpec holds a heredoc whose `=begin` line is text, not a block
// comment, and `#` inside regex literals, which is not a comment either.
const rbRegexSpec = `describe C do
  it "reads a heredoc" do
    text = <<-DOC
=begin
    DOC
    expect(text).to match(/#/)
    expect(text).to match(%r{#})
  end

  it "still counts" do
    expect(1).to eq(1)
  end
end
`

const goMain = `package a

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
`

const pyTests = `import pytest


def test_one():
    assert 1 == 1


def test_two():
    assert 2 == 2
`

const pyUnittest = `import unittest


class T(unittest.TestCase):
    def test_one(self):
        self.assertEqual(1, 1)
`

const rbTests = `describe A do
  it "one" do
    expect(1).to eq(1)
  end

  it "two" do
    2.should eq(2)
  end
end
`

// testsIntactBase is every fixture file; each case starts from all of them
// committed at the pinned base.
var testsIntactBase = map[string]string{
	"pkg/a_test.go":      goTests,
	"pkg/skip_test.go":   goSkipped,
	"pkg/main_test.go":   goMain,
	"pkg/open_test.go":   goTests + "/* never closed\n",
	"pkg/nul_test.go":    goTests + "\x00",
	"src/re.test.ts":     jsRegexTests,
	"spec/re_spec.rb":    rbRegexSpec,
	"pkg/a.go":           "package a\n",
	"src/a.test.ts":      jsTests,
	"tests/test_a.py":    pyTests,
	"tests/test_unit.py": pyUnittest,
	"spec/a_spec.rb":     rbTests,
	"src/helpers.py":     "def helper():\n    return 1\n",
}

type intactEdit struct {
	write  map[string]string
	remove []string
}

func testsIntactRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range testsIntactBase {
		writeIntactFile(t, dir, name, content)
	}
	intactGit(t, dir, "init", "--quiet")
	intactGit(t, dir, "add", "-A")
	intactGit(t, dir, "-c", "user.email=test@jig.local", "-c", "user.name=jig test",
		"commit", "--quiet", "-m", "base")
	return dir, strings.TrimSpace(intactGit(t, dir, "rev-parse", "HEAD"))
}

func intactGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func writeIntactFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func applyIntactEdit(t *testing.T, dir string, edit *intactEdit) {
	t.Helper()
	if edit == nil {
		return
	}
	for name, content := range edit.write {
		writeIntactFile(t, dir, name, content)
	}
	for _, name := range edit.remove {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// intactViolations reduces a report's failed checks to "path kind" pairs,
// so a case states WHICH file lost WHAT without pinning git's hunk choice.
func intactViolations(report protocol.GateReport) []string {
	var out []string
	for _, check := range report.Checks {
		if check.Ok {
			continue
		}
		file, _, _ := strings.Cut(check.Item, ":")
		kind := "?"
		for _, candidate := range []string{"unterminated", "uninspectable", "too large", "embedded", "index flag", "m.Run",
			"deleted", "test function", "assertion", "skip"} {
			if strings.Contains(check.Note, candidate) {
				kind = candidate
				break
			}
		}
		out = append(out, file+" "+kind)
	}
	sort.Strings(out)
	return out
}

func TestTestsIntactFlagsWeakenedTestsAndPassesHonestChanges(t *testing.T) {
	replace := func(name, old, new string) map[string]string {
		return map[string]string{name: strings.Replace(testsIntactBase[name], old, new, 1)}
	}
	cases := []struct {
		name        string
		committed   *intactEdit
		uncommitted *intactEdit
		allow       []string
		want        []string
	}{
		// (a) deletion, seen whether or not it was committed.
		{name: "an uncommitted deleted test file",
			uncommitted: &intactEdit{remove: []string{"pkg/a_test.go"}},
			want:        []string{"pkg/a_test.go deleted"}},
		{name: "a committed deleted test file",
			committed: &intactEdit{remove: []string{"tests/test_a.py"}},
			want:      []string{"tests/test_a.py deleted"}},
		// (b) net loss of test functions — a lost test takes its assertion.
		{name: "a Go test removed",
			uncommitted: &intactEdit{write: replace("pkg/a_test.go",
				"\nfunc TestTwo(t *testing.T) {\n\tif 2 != 2 {\n\t\tt.Errorf(\"two\")\n\t}\n}\n", "")},
			want: []string{"pkg/a_test.go assertion", "pkg/a_test.go test function"}},
		{name: "a Go test commented out",
			uncommitted: &intactEdit{write: replace("pkg/a_test.go",
				"func TestTwo(t *testing.T) {\n\tif 2 != 2 {\n\t\tt.Errorf(\"two\")\n\t}\n}\n",
				"// func TestTwo(t *testing.T) {\n// \tif 2 != 2 {\n// \t\tt.Errorf(\"two\")\n// \t}\n// }\n")},
			want: []string{"pkg/a_test.go assertion", "pkg/a_test.go test function"}},
		{name: "a Python test removed",
			committed: &intactEdit{write: replace("tests/test_a.py", "\n\ndef test_two():\n    assert 2 == 2\n", "")},
			want:      []string{"tests/test_a.py assertion", "tests/test_a.py test function"}},
		{name: "an RSpec example turned into xit",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb", `  it "two" do`, `  xit "two" do`)},
			want:        []string{"spec/a_spec.rb skip", "spec/a_spec.rb test function"}},
		// (c) net loss of assertions.
		{name: "a JS expect removed",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", "    expect(1).toBe(1);\n", "")},
			want:        []string{"src/a.test.ts assertion"}},
		{name: "a unittest assertion removed",
			uncommitted: &intactEdit{write: replace("tests/test_unit.py", "        self.assertEqual(1, 1)\n", "        pass\n")},
			want:        []string{"tests/test_unit.py assertion"}},
		{name: "an RSpec expectation removed",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb", "    expect(1).to eq(1)\n", "")},
			want:        []string{"spec/a_spec.rb assertion"}},
		// (d) added skip and focus markers.
		{name: "a Go t.Skip added",
			uncommitted: &intactEdit{write: replace("pkg/a_test.go",
				"func TestOne(t *testing.T) {\n", "func TestOne(t *testing.T) {\n\tt.Skip(\"flaky\")\n")},
			want: []string{"pkg/a_test.go skip"}},
		{name: "a JS describe.skip",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `describe("a"`, `describe.skip("a"`)},
			want:        []string{"src/a.test.ts skip"}},
		// A focused test is still a test: the focus is the finding.
		{name: "a JS test.only",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `  test("two"`, `  test.only("two"`)},
			want:        []string{"src/a.test.ts skip"}},
		{name: "a pytest skip decorator",
			committed: &intactEdit{write: replace("tests/test_a.py",
				"def test_two():", "@pytest.mark.skip(reason=\"flaky\")\ndef test_two():")},
			want: []string{"tests/test_a.py skip"}},
		{name: "an RSpec pending",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb",
				"    expect(1).to eq(1)\n", "    pending \"flaky\"\n    expect(1).to eq(1)\n")},
			want: []string{"spec/a_spec.rb skip"}},
		// Parameterized JS focus and skip, anywhere in the chain.
		{name: "a JS test.only.each",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", "test.each([1, 2])", "test.only.each([1, 2])")},
			want:        []string{"src/a.test.ts skip"}},
		{name: "a JS test.skip.each",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", "test.each([1, 2])", "test.skip.each([1, 2])")},
			want:        []string{"src/a.test.ts skip"}},
		{name: "a JS it.only.each",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `  it("one"`, `  it.only.each([1])("one"`)},
			want:        []string{"src/a.test.ts skip"}},
		{name: "a JS describe.only.each",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `describe("a"`, `describe.only.each([1])("a"`)},
			want:        []string{"src/a.test.ts skip"}},
		{name: "a JS describe.skip.each",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `describe("a"`, `describe.skip.each([1])("a"`)},
			want:        []string{"src/a.test.ts skip"}},
		// Markers that drop a file or end the run, not just skip one test.
		{name: "a go:build ignore constraint",
			uncommitted: &intactEdit{write: map[string]string{"pkg/a_test.go": "//go:build ignore\n\n" + goTests}},
			want:        []string{"pkg/a_test.go skip"}},
		{name: "a legacy +build constraint",
			uncommitted: &intactEdit{write: map[string]string{"pkg/a_test.go": "// +build ignore\n\n" + goTests}},
			want:        []string{"pkg/a_test.go skip"}},
		{name: "an added TestMain",
			uncommitted: &intactEdit{write: map[string]string{"pkg/a_test.go": goTests + "\nfunc TestMain(m *testing.M) {}\n"}},
			want:        []string{"pkg/a_test.go skip"}},
		{name: "an added os.Exit",
			uncommitted: &intactEdit{write: replace("pkg/a_test.go",
				"func TestOne(t *testing.T) {\n", "func TestOne(t *testing.T) {\n\tos.Exit(0)\n")},
			want: []string{"pkg/a_test.go skip"}},
		{name: "a module-level pytestmark",
			uncommitted: &intactEdit{write: replace("tests/test_a.py",
				"import pytest\n", "import pytest\n\npytestmark = pytest.mark.skip\n")},
			want: []string{"tests/test_a.py skip"}},
		{name: "a parenthesized pytest skipif",
			uncommitted: &intactEdit{write: replace("tests/test_a.py",
				"def test_two():", "@(pytest.mark.skipif(True, reason=\"x\"))\ndef test_two():")},
			want: []string{"tests/test_a.py skip"}},
		{name: "an aliased pytest mark",
			uncommitted: &intactEdit{write: replace("tests/test_a.py",
				"import pytest\n", "import pytest\nfrom pytest import mark\nskip_it = mark.skip\n")},
			want: []string{"tests/test_a.py skip"}},
		{name: "an xfail through pytest.param",
			uncommitted: &intactEdit{write: replace("tests/test_a.py", "def test_two():",
				"@pytest.mark.parametrize(\"n\", [pytest.param(1, marks=pytest.mark.xfail)])\ndef test_two():")},
			want: []string{"tests/test_a.py skip"}},
		{name: "a committed conftest collect_ignore",
			committed: &intactEdit{write: map[string]string{"conftest.py": "collect_ignore = [\"tests/test_a.py\"]\n"}},
			want:      []string{"conftest.py skip"}},
		{name: "an RSpec inline skip after do;",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb", "\nend\n", "\n  it \"three\" do; skip; end\nend\n")},
			want:        []string{"spec/a_spec.rb skip"}},
		{name: "an RSpec skip in a one-line block",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb", "\nend\n", "\n  it(\"four\") { skip }\nend\n")},
			want:        []string{"spec/a_spec.rb skip"}},
		// Untracked files: git diff never shows them.
		{name: "an untracked test file with a build constraint",
			uncommitted: &intactEdit{write: map[string]string{"pkg/c_test.go": "//go:build ignore\n\n" + goTests}},
			want:        []string{"pkg/c_test.go skip"}},
		{name: "an untracked conftest collect_ignore",
			uncommitted: &intactEdit{write: map[string]string{"tests/conftest.py": "collect_ignore = [\"test_a.py\"]\n"}},
			want:        []string{"tests/conftest.py skip"}},
		{name: "an untracked test file without markers",
			uncommitted: &intactEdit{write: map[string]string{"pkg/c_test.go": goTests, "src/c.spec.js": jsTests}}},
		{name: "an untracked test file the allow list covers",
			uncommitted: &intactEdit{write: map[string]string{"pkg/c_test.go": "//go:build ignore\n\n" + goTests}},
			allow:       []string{"pkg/c_test.go"}},
		// A -diff attribute must not hide a weakened file as binary.
		{name: "an untracked -diff attribute",
			uncommitted: &intactEdit{write: map[string]string{
				".gitattributes": "*_test.go -diff\n",
				"pkg/a_test.go":  strings.Replace(goTests, "\t\tt.Fatal(\"one\")\n", "", 1)}},
			want: []string{"pkg/a_test.go assertion"}},
		{name: "a committed -diff attribute",
			committed: &intactEdit{write: map[string]string{
				".gitattributes": "*.ts binary\n",
				"src/a.test.ts":  strings.Replace(jsTests, "    expect(1).toBe(1);\n", "", 1)}},
			want: []string{"src/a.test.ts assertion"}},
		// A reworded skip is the same skip.
		{name: "a skip message edited",
			uncommitted: &intactEdit{write: replace("pkg/skip_test.go", "old reason", "new reason")}},
		{name: "a skip moved to another test",
			uncommitted: &intactEdit{write: map[string]string{"pkg/skip_test.go": strings.Replace(goSkipped,
				"\tt.Skip(\"old reason\")\n}\n", "}\n\nfunc TestOther(t *testing.T) {\n\tt.Skip(\"other reason\")\n}\n", 1)}}},
		// Whole-file comparison: block comments, docstrings, and tokens
		// split across lines.
		{name: "a Go test wrapped in a block comment",
			uncommitted: &intactEdit{write: map[string]string{"pkg/a_test.go": strings.Replace(goTests,
				"func TestTwo", "/*\nfunc TestTwo", 1) + "*/\n"}},
			want: []string{"pkg/a_test.go assertion", "pkg/a_test.go test function"}},
		{name: "a skip after a closing block comment",
			uncommitted: &intactEdit{write: replace("pkg/a_test.go",
				"func TestOne(t *testing.T) {\n", "func TestOne(t *testing.T) {\n\t/* note */ t.Skip()\n")},
			want: []string{"pkg/a_test.go skip"}},
		{name: "a Go skip split across lines",
			uncommitted: &intactEdit{write: replace("pkg/a_test.go",
				"func TestOne(t *testing.T) {\n", "func TestOne(t *testing.T) {\n\tt.\n\t\tSkip(\"x\")\n")},
			want: []string{"pkg/a_test.go skip"}},
		{name: "a JS only split across lines",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `  test("two"`, "  test\n    .only(\"two\"")},
			want:        []string{"src/a.test.ts skip"}},
		{name: "a JS test converted to test.each",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `  test("two", () => {`, `  test.each([2])("two", (n) => {`)}},
		{name: "a Python test wrapped in a string",
			uncommitted: &intactEdit{write: replace("tests/test_a.py",
				"def test_two():\n    assert 2 == 2\n", "\"\"\"\ndef test_two():\n    assert 2 == 2\n\"\"\"\n")},
			want: []string{"tests/test_a.py assertion", "tests/test_a.py test function"}},
		{name: "an RSpec example in =begin/=end",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb",
				"  it \"two\" do\n    2.should eq(2)\n  end\n", "=begin\n  it \"two\" do\n    2.should eq(2)\n  end\n=end\n")},
			want: []string{"spec/a_spec.rb assertion", "spec/a_spec.rb test function"}},
		// TestMain must keep running the suite.
		{name: "os.Exit(m.Run()) replaced by os.Exit(0)",
			uncommitted: &intactEdit{write: replace("pkg/main_test.go", "os.Exit(m.Run())", "os.Exit(0)")},
			want:        []string{"pkg/main_test.go m.Run"}},
		// More Python and RSpec markers.
		{name: "a pytest.importorskip",
			uncommitted: &intactEdit{write: replace("tests/test_a.py",
				"import pytest\n", "import pytest\n\nnp = pytest.importorskip(\"numpy\")\n")},
			want: []string{"tests/test_a.py skip"}},
		{name: "a module-level __test__ = False",
			uncommitted: &intactEdit{write: replace("tests/test_a.py", "import pytest\n", "import pytest\n\n__test__ = False\n")},
			want:        []string{"tests/test_a.py skip"}},
		{name: "an annotated pytestmark",
			uncommitted: &intactEdit{write: replace("tests/test_a.py",
				"import pytest\n", "import pytest\n\npytestmark: list = [marker]\n")},
			want: []string{"tests/test_a.py skip"}},
		{name: "a mark imported under an alias",
			uncommitted: &intactEdit{write: replace("tests/test_a.py",
				"import pytest\n", "import pytest\nfrom pytest import mark as m\n")},
			want: []string{"tests/test_a.py skip"}},
		{name: "a conftest pytest_ignore_collect",
			uncommitted: &intactEdit{write: map[string]string{"tests/conftest.py": "def pytest_ignore_collect(collection_path):\n    return True\n"}},
			want:        []string{"tests/conftest.py skip"}},
		{name: "a conftest pytest_collection_modifyitems",
			committed: &intactEdit{write: map[string]string{"conftest.py": "def pytest_collection_modifyitems(items):\n    items.clear()\n"}},
			want:      []string{"conftest.py skip"}},
		{name: "RSpec skip: true metadata",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb", `  it "two" do`, `  it "two", skip: true do`)},
			want:        []string{"spec/a_spec.rb skip"}},
		{name: "RSpec :skip metadata",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb", `  it "two" do`, `  it "two", :skip do`)},
			want:        []string{"spec/a_spec.rb skip"}},
		{name: "RSpec pending: true metadata",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb", `  it "two" do`, `  it "two", pending: true do`)},
			want:        []string{"spec/a_spec.rb skip"}},
		{name: "RSpec focus: true metadata",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb", `describe A do`, `describe A, focus: true do`)},
			want:        []string{"spec/a_spec.rb skip"}},
		// Gitignored files still run; installed dependencies are not ours.
		{name: "a gitignored untracked test file with a marker",
			uncommitted: &intactEdit{write: map[string]string{
				".gitignore":         "pkg/hidden_test.go\n",
				"pkg/hidden_test.go": "//go:build ignore\n\n" + goTests}},
			want: []string{"pkg/hidden_test.go skip"}},
		{name: "a focused test inside node_modules",
			uncommitted: &intactEdit{write: map[string]string{
				".gitignore":                 "node_modules/\n",
				"node_modules/dep/a.test.js": "describe.only(\"dep\", () => {});\n"}}},
		// Regex literals, heredocs, and %-literals are not comments.
		{name: "an assertion removed after a JS regex holding /*",
			uncommitted: &intactEdit{write: replace("src/re.test.ts", "  expect(cls.test(\"*\")).toBe(true);\n", "")},
			want:        []string{"src/re.test.ts assertion"}},
		{name: "an assertion removed after a Ruby heredoc holding =begin",
			uncommitted: &intactEdit{write: replace("spec/re_spec.rb", "    expect(1).to eq(1)\n", "")},
			want:        []string{"spec/re_spec.rb assertion"}},
		{name: "a pending hidden after # in a Ruby regex",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb",
				"    expect(1).to eq(1)\n", "    expect(\"#\").to match(/#/); pending\n    expect(1).to eq(1)\n")},
			want: []string{"spec/a_spec.rb skip"}},
		{name: "a pending hidden after # in a Ruby %r literal",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb",
				"    expect(1).to eq(1)\n", "    expect(\"#\").to match(%r{#}); pending\n    expect(1).to eq(1)\n")},
			want: []string{"spec/a_spec.rb skip"}},
		{name: "a Go block comment left open",
			uncommitted: &intactEdit{write: map[string]string{"pkg/a_test.go": goTests + "/*\n"}},
			want:        []string{"pkg/a_test.go unterminated"}},
		{name: "a JS template literal left open",
			uncommitted: &intactEdit{write: map[string]string{"src/a.test.ts": jsTests + "const s = `open\n"}},
			want:        []string{"src/a.test.ts unterminated"}},
		{name: "a Ruby heredoc left open",
			uncommitted: &intactEdit{write: map[string]string{"spec/a_spec.rb": rbTests + "text = <<~EOS\nnever ends\n"}},
			want:        []string{"spec/a_spec.rb unterminated"}},
		{name: "a base version left open",
			uncommitted: &intactEdit{write: map[string]string{"pkg/open_test.go": goTests}},
			want:        []string{"pkg/open_test.go unterminated"}},
		// Bare unittest skips.
		{name: "a bare @skip from unittest",
			uncommitted: &intactEdit{write: replace("tests/test_unit.py",
				"import unittest\n", "import unittest\nfrom unittest import skip\n")}},
		{name: "a bare @skip decorator",
			uncommitted: &intactEdit{write: replace("tests/test_unit.py",
				"    def test_one(self):", "    @skip(\"flaky\")\n    def test_one(self):")},
			want: []string{"tests/test_unit.py skip"}},
		{name: "a bare @skipIf decorator",
			uncommitted: &intactEdit{write: replace("tests/test_unit.py",
				"    def test_one(self):", "    @skipIf(True, \"flaky\")\n    def test_one(self):")},
			want: []string{"tests/test_unit.py skip"}},
		{name: "a bare @skipUnless decorator",
			uncommitted: &intactEdit{write: replace("tests/test_unit.py",
				"    def test_one(self):", "    @skipUnless(False, \"flaky\")\n    def test_one(self):")},
			want: []string{"tests/test_unit.py skip"}},
		{name: "a bare @expectedFailure decorator",
			uncommitted: &intactEdit{write: replace("tests/test_unit.py",
				"    def test_one(self):", "    @expectedFailure\n    def test_one(self):")},
			want: []string{"tests/test_unit.py skip"}},
		{name: "raise unittest.SkipTest",
			uncommitted: &intactEdit{write: replace("tests/test_unit.py",
				"        self.assertEqual(1, 1)\n", "        raise unittest.SkipTest(\"x\")\n        self.assertEqual(1, 1)\n")},
			want: []string{"tests/test_unit.py skip"}},
		{name: "raise a bare SkipTest",
			uncommitted: &intactEdit{write: replace("tests/test_unit.py",
				"        self.assertEqual(1, 1)\n", "        raise SkipTest(\"x\")\n        self.assertEqual(1, 1)\n")},
			want: []string{"tests/test_unit.py skip"}},
		{name: "a unittest skip imported under an alias",
			uncommitted: &intactEdit{write: replace("tests/test_unit.py",
				"import unittest\n", "import unittest\nfrom unittest import skip as later\n")},
			want: []string{"tests/test_unit.py skip"}},
		// vitest chain modifiers.
		{name: "a vitest test.skipIf",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `  test("two"`, `  test.skipIf(true)("two"`)},
			want:        []string{"src/a.test.ts skip"}},
		{name: "a vitest it.runIf",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `  it("one"`, `  it.runIf(false)("one"`)},
			want:        []string{"src/a.test.ts skip"}},
		{name: "a vitest test.fails",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `  test("two"`, `  test.fails("two"`)},
			want:        []string{"src/a.test.ts skip"}},
		{name: "a vitest it.concurrent.only",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `  it("one"`, `  it.concurrent.only("one"`)},
			want:        []string{"src/a.test.ts skip"}},
		// NUL bytes and wide encodings cannot be read as source.
		{name: "a NUL byte in a tracked worktree file",
			uncommitted: &intactEdit{write: map[string]string{"pkg/a_test.go": goTests + "\x00"}},
			want:        []string{"pkg/a_test.go uninspectable"}},
		{name: "a NUL byte in the base version",
			uncommitted: &intactEdit{write: map[string]string{"pkg/nul_test.go": goTests}},
			want:        []string{"pkg/nul_test.go uninspectable"}},
		{name: "a NUL byte in an untracked test file",
			uncommitted: &intactEdit{write: map[string]string{"pkg/z_test.go": "package a\n\x00func TestZ(t *testing.T) { t.Skip() }\n"}},
			want:        []string{"pkg/z_test.go uninspectable"}},
		{name: "a UTF-16 byte-order mark",
			uncommitted: &intactEdit{write: map[string]string{"pkg/w_test.go": "\xFF\xFEpackage a\n"}},
			want:        []string{"pkg/w_test.go uninspectable"}},
		{name: "a binary fixture under tests/",
			uncommitted: &intactEdit{write: map[string]string{"tests/fixtures/image.bin": "\x89PNG\x00\x01"}}},
		{name: "committed and uncommitted losses are both seen",
			committed:   &intactEdit{write: replace("src/a.test.ts", "    expect(1).toBe(1);\n", "")},
			uncommitted: &intactEdit{remove: []string{"spec/a_spec.rb"}},
			want:        []string{"spec/a_spec.rb deleted", "src/a.test.ts assertion"}},
		// Renames: --no-renames makes the old path a deletion.
		{name: "a renamed test file is a deletion",
			committed: &intactEdit{remove: []string{"pkg/a_test.go"},
				write: map[string]string{"pkg/b_test.go": goTests}},
			want: []string{"pkg/a_test.go deleted"}},
		{name: "a renamed test file the allow list covers",
			committed: &intactEdit{remove: []string{"pkg/a_test.go"},
				write: map[string]string{"pkg/b_test.go": goTests}},
			allow: []string{"pkg/a_test.go"}},
		{name: "allow exempts every loss in a matching path",
			uncommitted: &intactEdit{remove: []string{"tests/test_a.py"},
				write: replace("tests/test_unit.py", "        self.assertEqual(1, 1)\n", "")},
			allow: []string{"tests/test_*.py"}},
		{name: "allow exempts only what it matches",
			uncommitted: &intactEdit{remove: []string{"tests/test_a.py", "spec/a_spec.rb"}},
			allow:       []string{"tests/*"},
			want:        []string{"spec/a_spec.rb deleted"}},
		// Honest changes pass.
		{name: "an assertion replaced with a stronger one",
			uncommitted: &intactEdit{write: replace("pkg/a_test.go",
				"\tif 1 != 1 {\n\t\tt.Fatal(\"one\")\n\t}\n", "\trequire.Equal(t, 1, 1)\n")}},
		{name: "a test renamed within its file",
			uncommitted: &intactEdit{write: replace("tests/test_a.py", "def test_two():", "def test_two_is_two():")}},
		{name: "a new test file",
			committed: &intactEdit{write: map[string]string{"pkg/c_test.go": goTests, "src/c.spec.js": jsTests}}},
		{name: "new tests appended",
			uncommitted: &intactEdit{write: map[string]string{"pkg/a_test.go": goTests +
				"\nfunc TestThree(t *testing.T) {\n\tif 3 != 3 {\n\t\tt.Fatal(\"three\")\n\t}\n}\n"}}},
		{name: "a skip marker in a non-test file",
			uncommitted: &intactEdit{write: map[string]string{
				"pkg/a.go":       "package a\n\nfunc helper(t T) { t.Skip() }\n",
				"src/helpers.py": "@pytest.mark.skip\ndef helper():\n    return 1\n"}}},
		{name: "a commented-out skip marker",
			uncommitted: &intactEdit{write: replace("pkg/a_test.go",
				"func TestOne(t *testing.T) {\n", "func TestOne(t *testing.T) {\n\t// t.Skip(\"flaky\")\n")}},
		{name: "nothing changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, base := testsIntactRepo(t)
			if tc.committed != nil {
				applyIntactEdit(t, dir, tc.committed)
				intactGit(t, dir, "add", "-A")
				intactGit(t, dir, "-c", "user.email=test@jig.local", "-c", "user.name=jig test",
					"commit", "--quiet", "-m", "agent")
			}
			applyIntactEdit(t, dir, tc.uncommitted)
			report := gateTestsIntact(gateContext{ctx: context.Background(), worktree: dir,
				baseSHA: base, allow: tc.allow})
			got := intactViolations(report)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("violations = %q, want %q\nchecks: %+v", got, tc.want, report.Checks)
			}
			if len(report.Checks) == 0 {
				t.Fatal("a green gate must still record what it checked (R9)")
			}
		})
	}
}

// Findings name the worktree line, so the correction points the agent at
// the spot; the note says what to do about it.
func TestTestsIntactFindingsCarryTheLineAndTheFix(t *testing.T) {
	dir, base := testsIntactRepo(t)
	writeIntactFile(t, dir, "pkg/a_test.go", strings.Replace(goTests,
		"func TestTwo(t *testing.T) {\n", "func TestTwo(t *testing.T) {\n\tt.Skip(\"flaky\")\n", 1))
	writeIntactFile(t, dir, "src/a.test.ts", strings.Replace(jsTests, "    expect(1).toBe(1);\n", "", 1))
	report := gateTestsIntact(gateContext{ctx: context.Background(), worktree: dir, baseSHA: base})
	want := map[string]string{
		"pkg/a_test.go:12": `adds a skip/focus marker (t.Skip("flaky"))`,
		"src/a.test.ts:3":  "net loss of 1 assertion(s) (3 at base " + shortSHA(base) + ", 2 now)",
	}
	for _, check := range report.Checks {
		if check.Ok {
			continue
		}
		note, ok := want[check.Item]
		if !ok || !strings.Contains(check.Note, note) {
			t.Fatalf("unexpected failed check %+v", check)
		}
		delete(want, check.Item)
	}
	if len(want) > 0 {
		t.Fatalf("missing findings %v in %+v", want, report.Checks)
	}
}

// A repository's diff.interHunkContext would merge nearby hunks and fill the
// gap with context lines; the gate forces it off and counts context anyway,
// so the second finding still names its own line.
func TestTestsIntactLineNumbersSurviveInterHunkContext(t *testing.T) {
	dir, base := testsIntactRepo(t)
	intactGit(t, dir, "config", "diff.interHunkContext", "20")
	skipped := strings.Replace(goTests, "func TestOne(t *testing.T) {\n",
		"func TestOne(t *testing.T) {\n\tt.Skip(\"one\")\n", 1)
	skipped = strings.Replace(skipped, "func TestTwo(t *testing.T) {\n",
		"func TestTwo(t *testing.T) {\n\tt.Skip(\"two\")\n", 1)
	writeIntactFile(t, dir, "pkg/a_test.go", skipped)
	report := gateTestsIntact(gateContext{ctx: context.Background(), worktree: dir, baseSHA: base})
	var items []string
	for _, check := range report.Checks {
		if !check.Ok {
			items = append(items, check.Item)
		}
	}
	if want := []string{"pkg/a_test.go:6", "pkg/a_test.go:13"}; strings.Join(items, ",") != strings.Join(want, ",") {
		t.Fatalf("failed items = %q, want %q", items, want)
	}
	// The parser alone, on a hunk that carries context: the counter must
	// advance over it.
	files := parseUnifiedDiff("diff --git a/x_test.go b/x_test.go\n--- a/x_test.go\n+++ b/x_test.go\n" +
		"@@ -5,3 +5,4 @@\n func TestA(t *testing.T) {\n+\tt.Skip()\n \tt.Fatal(\"a\")\n }\n")
	if len(files) != 1 || len(files[0].added) != 1 || files[0].added[0].line != 6 {
		t.Fatalf("parsed %+v, want one added line at 6", files)
	}
}

// Should git ever report a test file as binary despite --text, the gate
// refuses to vouch for what it cannot read.
func TestTestsIntactFailsABinaryTestFile(t *testing.T) {
	files := parseUnifiedDiff("diff --git a/pkg/a_test.go b/pkg/a_test.go\nindex 1111111..2222222 100644\n" +
		"Binary files a/pkg/a_test.go and b/pkg/a_test.go differ\n")
	var report protocol.GateReport
	(&intactJudge{report: &report, base: "base"}).judgeDiffFile(files[0])
	if report.Passed() || report.Checks[0].Item != "pkg/a_test.go" ||
		!strings.Contains(report.Checks[0].Note, "binary") {
		t.Fatalf("checks = %+v, want a failed binary check", report.Checks)
	}
	report = protocol.GateReport{}
	(&intactJudge{gc: gateContext{allow: []string{"pkg/*"}}, report: &report, base: "base"}).judgeDiffFile(files[0])
	if !report.Passed() {
		t.Fatalf("checks = %+v, want allow to exempt it", report.Checks)
	}
}

// What the gate cannot read fails closed: an oversized file (never
// truncated), an embedded repository git shows as one opaque entry, and a
// tracked file whose index flags make git diff report it unchanged.
func TestTestsIntactFailsClosedOnWhatItCannotInspect(t *testing.T) {
	padding := strings.Repeat("// padding line\n", maxTestFileBytes/16+1)
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  []string
	}{
		{"an oversized tracked test file", func(t *testing.T, dir string) {
			writeIntactFile(t, dir, "pkg/a_test.go", goTests+padding+"func init() { _ = 1 }\n")
		}, []string{"pkg/a_test.go too large"}},
		{"an oversized untracked test file", func(t *testing.T, dir string) {
			writeIntactFile(t, dir, "pkg/big_test.go", goTests+padding)
		}, []string{"pkg/big_test.go too large"}},
		{"an embedded repository holding tests", func(t *testing.T, dir string) {
			writeIntactFile(t, dir, "sub/x_test.go", goTests)
			intactGit(t, filepath.Join(dir, "sub"), "init", "--quiet")
		}, []string{"sub/ embedded"}},
		{"skip-worktree on a weakened test file", func(t *testing.T, dir string) {
			intactGit(t, dir, "update-index", "--skip-worktree", "pkg/a_test.go")
			writeIntactFile(t, dir, "pkg/a_test.go", strings.Replace(goTests, "\t\tt.Fatal(\"one\")\n", "", 1))
		}, []string{"pkg/a_test.go index flag"}},
		{"assume-unchanged on a weakened test file", func(t *testing.T, dir string) {
			intactGit(t, dir, "update-index", "--assume-unchanged", "tests/test_a.py")
			writeIntactFile(t, dir, "tests/test_a.py", strings.Replace(pyTests, "    assert 2 == 2\n", "    pass\n", 1))
		}, []string{"tests/test_a.py index flag"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, base := testsIntactRepo(t)
			tc.setup(t, dir)
			report := gateTestsIntact(gateContext{ctx: context.Background(), worktree: dir, baseSHA: base})
			got := intactViolations(report)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("violations = %q, want %q\nchecks: %+v", got, tc.want, report.Checks)
			}
		})
	}
}

func TestTestsIntactFailsClosedWithoutABase(t *testing.T) {
	dir, _ := testsIntactRepo(t)
	cases := []struct {
		name, base, item string
	}{
		{"no pinned base", "", "base commit"},
		{"a base git cannot resolve", strings.Repeat("0", 40), "git diff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := gateTestsIntact(gateContext{ctx: context.Background(), worktree: dir, baseSHA: tc.base})
			if report.Passed() || report.Checks[0].Item != tc.item {
				t.Fatalf("checks = %+v, want a failed %q check", report.Checks, tc.item)
			}
		})
	}
}

// A path with spaces or characters git quotes must still be classified by
// its real name, or a deletion would slip past as a non-test path.
func TestTestsIntactReadsQuotedAndSpacedPaths(t *testing.T) {
	dir, base := testsIntactRepo(t)
	for _, name := range []string{"pkg/with space_test.go", "pkg/tab\there_test.go"} {
		writeIntactFile(t, dir, name, goTests)
	}
	intactGit(t, dir, "add", "-A")
	intactGit(t, dir, "-c", "user.email=test@jig.local", "-c", "user.name=jig test",
		"commit", "--quiet", "-m", "more")
	base = strings.TrimSpace(intactGit(t, dir, "rev-parse", "HEAD"))
	for _, name := range []string{"pkg/with space_test.go", "pkg/tab\there_test.go"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	report := gateTestsIntact(gateContext{ctx: context.Background(), worktree: dir, baseSHA: base})
	got := intactViolations(report)
	want := []string{"pkg/tab\there_test.go deleted", "pkg/with space_test.go deleted"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("violations = %q, want %q", got, want)
	}
}
