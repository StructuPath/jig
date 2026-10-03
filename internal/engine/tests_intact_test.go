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
		for _, candidate := range []string{"deleted", "test function", "assertion", "skip"} {
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
		{name: "a JS test.only",
			uncommitted: &intactEdit{write: replace("src/a.test.ts", `  test("two"`, `  test.only("two"`)},
			want:        []string{"src/a.test.ts skip", "src/a.test.ts test function"}},
		{name: "a pytest skip decorator",
			committed: &intactEdit{write: replace("tests/test_a.py",
				"def test_two():", "@pytest.mark.skip(reason=\"flaky\")\ndef test_two():")},
			want: []string{"tests/test_a.py skip"}},
		{name: "an RSpec pending",
			uncommitted: &intactEdit{write: replace("spec/a_spec.rb",
				"    expect(1).to eq(1)\n", "    pending \"flaky\"\n    expect(1).to eq(1)\n")},
			want: []string{"spec/a_spec.rb skip"}},
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
		"src/a.test.ts:3":  "net loss of 1 assertion(s) (removed 1, added 0)",
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
