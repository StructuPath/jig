// factory_test.go — the stock factory's risk classifier, run as written in
// examples/definitions/factory.yaml. It decides what ships without a person,
// and with CI repair it is what holds a "fix" that weakens a check instead
// of the code, so its script is tested rather than trusted.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/StructuPath/jig/internal/protocol"
)

func classifyRiskCommand(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile(stockDefinition("factory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := protocol.ParseDefinition(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range spec.Phases {
		if phase.Name == "classify-risk" {
			return phase.Command
		}
	}
	t.Fatal("factory.yaml has no classify-risk phase")
	return ""
}

// classify commits base, applies change on top, and runs the classifier.
func classify(t *testing.T, base map[string]string, change func(dir string)) map[string]any {
	t.Helper()
	dir := t.TempDir()
	write := func(files map[string]string) {
		for name, body := range files {
			full := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	gitIn(t, dir, "init", "-q")
	write(map[string]string{"README.md": "hello\n"})
	write(base)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base")
	baseSHA := strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD"))
	change(dir)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "change")

	command := exec.Command("sh", "-c", classifyRiskCommand(t))
	command.Dir = dir
	command.Env = append(os.Environ(), "JIG_BASE_SHA="+baseSHA)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("classify-risk failed: %v\n%s", err, output)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var report map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		t.Fatalf("classify-risk's last line is not a report: %v\n%s", err, output)
	}
	return report
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const threeTests = "package app\n\nfunc TestA(t *testing.T) {}\nfunc TestB(t *testing.T) {}\nfunc TestC(t *testing.T) {}\n"

func TestTheFactoryClassifierHoldsChangesThatWeakenChecks(t *testing.T) {
	for _, tc := range []struct {
		name, risk, reason string
		base               map[string]string
		change             func(t *testing.T, dir string)
	}{
		{"an ordinary change", "low", "no sensitive paths", nil, func(t *testing.T, dir string) {
			writeFile(t, dir, "app.go", "package app\n")
		}},
		{"a change that adds tests", "low", "no sensitive paths", map[string]string{"app_test.go": "package app\n"},
			func(t *testing.T, dir string) { writeFile(t, dir, "app_test.go", threeTests) }},
		{"a change that deletes tests", "high", "removes more test lines", map[string]string{"app_test.go": threeTests},
			func(t *testing.T, dir string) { writeFile(t, dir, "app_test.go", "package app\n") }},
		{"a change that deletes a test file", "high", "removes more test lines", map[string]string{"tests/test_app.py": "def test_a():\n    assert True\n"},
			func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, "tests/test_app.py")) }},
		{"a change to lint configuration", "high", "sensitive paths", map[string]string{".golangci.yml": "linters:\n  enable: [errcheck]\n"},
			func(t *testing.T, dir string) { writeFile(t, dir, ".golangci.yml", "linters:\n  disable-all: true\n") }},
		{"a change anywhere under .github", "high", "sensitive paths", nil, func(t *testing.T, dir string) {
			writeFile(t, dir, ".github/CODEOWNERS", "* @someone\n")
		}},
		{"a change to a test runner config", "high", "sensitive paths", nil, func(t *testing.T, dir string) {
			writeFile(t, dir, "vitest.config.ts", "export default {}\n")
		}},
		{"a change to an auth path", "high", "sensitive paths", nil, func(t *testing.T, dir string) {
			writeFile(t, dir, "internal/auth/token.go", "package auth\n")
		}},
		{"a large change", "high", "exceeds threshold", nil, func(t *testing.T, dir string) {
			writeFile(t, dir, "big.txt", strings.Repeat("line\n", 450))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := classify(t, tc.base, func(dir string) { tc.change(t, dir) })
			if report["risk"] != tc.risk || !strings.Contains(report["risk_reason"].(string), tc.reason) {
				t.Fatalf("report = %v, want risk %s because %q", report, tc.risk, tc.reason)
			}
		})
	}
}

// The stock factory opts into CI repair, and its repair phase is the builder
// with every reviewer and the classifier after it.
func TestTheFactoryRepairsRedCIThroughItsWholeReviewPanel(t *testing.T) {
	source, err := os.ReadFile(stockDefinition("factory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := protocol.ParseDefinition(source)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.WaitsForCI() || spec.Publish.CI.OnFail == nil || spec.Publish.CI.OnFail.Run != "build" ||
		spec.Publish.CI.OnFail.Budget < 1 {
		t.Fatalf("publish.ci = %+v, want on_fail repairing through build", spec.Publish.CI)
	}
	// One re-run tells a flaky job from a broken one before a round is spent.
	if spec.Publish.CI.Rerun == nil || spec.Publish.CI.Rerun.Budget != 1 {
		t.Fatalf("publish.ci.rerun = %+v, want budget 1", spec.Publish.CI.Rerun)
	}
	after := map[string]bool{}
	seen := false
	for _, phase := range spec.Phases {
		if seen {
			after[phase.Name] = true
		}
		seen = seen || phase.Name == "build"
	}
	for _, judge := range []string{"test", "review-correctness", "review-security", "review-maintainability", "classify-risk"} {
		if !after[judge] {
			t.Fatalf("%s does not run after build, so it would not judge a CI repair", judge)
		}
	}
}

func parseStock(t *testing.T, name string) *protocol.DefinitionSpec {
	t.Helper()
	source, err := os.ReadFile(stockDefinition(name))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := protocol.ParseDefinition(source)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// The stock factory keeps its sequential panel (plan KTD7); the parallel
// example groups exactly its three reviewers and is otherwise the same
// definition, so the two cannot drift apart.
func TestTheParallelFactoryIsTheStockFactoryWithItsPanelGrouped(t *testing.T) {
	stock := parseStock(t, "factory.yaml")
	parallel := parseStock(t, "factory-parallel.yaml")
	if stock.Parallel != nil {
		t.Fatalf("factory.yaml declares parallel: %v; the stock factory stays sequential", stock.Parallel)
	}
	want := protocol.ParallelGroup{"review-correctness", "review-security", "review-maintainability"}
	if !reflect.DeepEqual(parallel.Parallel, want) {
		t.Fatalf("factory-parallel.yaml groups %v, want %v", parallel.Parallel, want)
	}
	parallel.Name, parallel.Parallel = stock.Name, nil
	if !reflect.DeepEqual(parallel, stock) {
		t.Fatal("factory-parallel.yaml differs from factory.yaml beyond its name and its parallel group")
	}
}
