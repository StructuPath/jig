package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// simpleSDLC is the plan's canonical shape (KTD2): an agent-phase repair
// edge keyed on `approved == false` targeting the builder with rerun-self, a
// retest phase guarded by `if: revised`, and a proceed-on-exhaustion review
// loop.
const simpleSDLC = `
name: simple-sdlc
roster:
  planner:
    model: claude-sonnet
    system_prompt_path: prompts/planner/system.md
    user_prompt_path: prompts/planner/user.md
    writes: ["docs/plans/"]
    env: ["ANTHROPIC_API_KEY"]
    sensitive_env: ["ANTHROPIC_API_KEY"]
  builder:
    model: claude-sonnet
    thinking: high
    system_prompt: You build exactly what the plan says.
    user_prompt: Implement the plan.
    tools: ["read", "write", "bash"]
    env: ["ANTHROPIC_API_KEY"]
    sensitive_env: ["ANTHROPIC_API_KEY"]
  reviewer:
    model: claude-sonnet
    system_prompt: You verify the build against the request.
    user_prompt: Review the change.
    writes: []
    env: ["ANTHROPIC_API_KEY"]
    sensitive_env: ["ANTHROPIC_API_KEY"]
phases:
  - name: plan
    kind: agent
    owner: planner
    gates:
      - name: artifacts_exist
      - name: files_non_empty
  - name: build
    kind: agent
    owner: builder
    gates:
      - name: diff_matches_claims
        budget: 2
  - name: test
    kind: code
    command: just check
    on_fail:
      run: build
      then: rerun-self
      budget: 2
  - name: review
    kind: agent
    owner: reviewer
    gates:
      - name: verdict_consistent
    on_fail:
      when: approved == false
      run: build
      then: rerun-self
      budget: 2
      exhausted: proceed
  - name: retest
    kind: code
    command: just check
    if: revised
acceptance:
  - all_phases_passed
  - verdict_consistent
`

func mustParse(t *testing.T, source string) *DefinitionSpec {
	t.Helper()
	spec, err := ParseDefinition([]byte(source))
	if err != nil {
		t.Fatalf("expected definition to validate, got: %v", err)
	}
	return spec
}

func mustReject(t *testing.T, source string, wantInMessage ...string) {
	t.Helper()
	_, err := ParseDefinition([]byte(source))
	if err == nil {
		t.Fatal("expected validation to reject the definition, got nil error")
	}
	for _, want := range wantInMessage {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validation message %q does not name %q", err.Error(), want)
		}
	}
}

func TestSimpleSDLCShapeValidates(t *testing.T) {
	spec := mustParse(t, simpleSDLC)
	review := spec.Phases[3]
	if review.Name != "review" || review.OnFail == nil {
		t.Fatalf("expected the review phase to carry a repair edge, got %+v", review)
	}
	predicate, err := ParsePredicate(review.OnFail.When)
	if err != nil {
		t.Fatalf("review repair predicate did not parse: %v", err)
	}
	if predicate.Field != "approved" || predicate.Op != "==" || predicate.Literal != "false" {
		t.Fatalf("expected `approved == false`, parsed %+v", predicate)
	}
	if review.OnFail.Run != "build" || review.OnFail.Then != RepairThenRerunSelf {
		t.Fatalf("expected the review loop to dispatch the builder then rerun-self, got %+v", review.OnFail)
	}
	if review.OnFail.Exhausted != RepairExhaustedProceed {
		t.Fatalf("expected a proceed-on-exhaustion review loop, got %q", review.OnFail.Exhausted)
	}
	retest := spec.Phases[4]
	if retest.Name != "retest" || retest.If != "revised" {
		t.Fatalf("expected retest guarded by `if: revised`, got %+v", retest)
	}
}

func TestUndefinedOwnerRoleIsRejectedNamingRoleAndPhase(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
phases:
  - name: plan
    kind: agent
    owner: planner
`, `"plan"`, `"planner"`, "not defined in the roster")
}

func TestUnresolvableGateNameIsRejectedNamingTheGate(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: builder
    gates:
      - name: lint_passes
`, `"build"`, `"lint_passes"`, "built-in registry")
}

func TestUnresolvableAcceptanceCheckIsRejectedNamingTheCheck(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: builder
acceptance:
  - tests_pass
`, `"tests_pass"`, "does not resolve")
}

func TestRepairEdgeWithoutBudgetIsRejectedAsUnbounded(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: builder
  - name: test
    kind: code
    command: just check
    on_fail:
      run: build
      then: rerun-self
`, `"test"`, "unbounded repair loops are rejected")
}

func TestRepairCycleBeyondDeclaredBudgetsIsRejectedNamingThePhases(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
  fixer:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: builder
    on_fail:
      when: approved == false
      run: fix
      then: rerun-self
      budget: 2
  - name: fix
    kind: agent
    owner: fixer
    on_fail:
      when: approved == false
      run: build
      then: rerun-self
      budget: 2
`, "repair cycle", `"build"`, `"fix"`)
}

func TestRepairEdgeTargetingUndefinedPhaseIsRejected(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
phases:
  - name: test
    kind: code
    command: just check
    on_fail:
      run: builder
      then: rerun-self
      budget: 2
`, `"test"`, `undefined phase "builder"`)
}

func TestAgentRepairEdgeWithoutPredicateIsRejected(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
  reviewer:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: builder
  - name: review
    kind: agent
    owner: reviewer
    on_fail:
      run: build
      then: rerun-self
      budget: 2
`, `"review"`, `"when" envelope predicate`)
}

func TestCodePhaseRepairEdgeWithPredicateIsRejected(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: builder
  - name: test
    kind: code
    command: just check
    on_fail:
      when: approved == false
      run: build
      then: rerun-self
      budget: 2
`, `"test"`, "nonzero exit")
}

func TestMissingPromptContentIsRejectedNamingTheRole(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: builder
`, `"builder"`, "missing system prompt content")
}

func TestTestsPassGateWithoutCommandIsRejected(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: builder
    gates:
      - name: tests_pass
`, `"build"`, `"tests_pass"`, "requires a command")
}

func TestTestsIntactAllowIsValidatedAtSaveTime(t *testing.T) {
	const base = `
name: tests-intact
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u}
phases:
  - name: build
    kind: agent
    owner: builder
    gates:
`
	spec := mustParse(t, base+`      - {name: tests_intact, allow: ["legacy/*_test.go", "e2e/[ab]*.spec.ts"]}`+"\n")
	if got := spec.Phases[0].Gates[0].Allow; len(got) != 2 || got[0] != "legacy/*_test.go" {
		t.Fatalf("allow = %q, want both globs kept in order", got)
	}
	mustParse(t, base+"      - {name: tests_intact}\n")
	cases := []struct {
		name string
		gate string
		want []string
	}{
		{"allow on another gate", `{name: artifacts_exist, allow: ["*_test.go"]}`,
			[]string{`"build"`, `"artifacts_exist"`, `only meaningful on "tests_intact"`}},
		{"an empty allow entry", `{name: tests_intact, allow: [""]}`,
			[]string{`"tests_intact"`, "non-empty"}},
		{"a blank allow entry", `{name: tests_intact, allow: ["  "]}`,
			[]string{`"tests_intact"`, "non-empty"}},
		{"an unclosed character class", `{name: tests_intact, allow: ["foo[_test.go"]}`,
			[]string{`"foo[_test.go"`, "malformed"}},
		{"a trailing escape", `{name: tests_intact, allow: ["foo\\"]}`,
			[]string{"malformed"}},
		{"a double star", `{name: tests_intact, allow: ["legacy/**"]}`,
			[]string{`"legacy/**"`, "not supported"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustReject(t, base+"      - "+tc.gate+"\n", tc.want...)
		})
	}
}

const buildOutputsBase = `
name: outputs
roster:
  reviewer: {model: opus, system_prompt: s, user_prompt: u, writes: []}
phases:
  - {name: review, kind: agent, owner: reviewer}
`

func TestBuildOutputsAreParsedAndRetained(t *testing.T) {
	spec := mustParse(t, buildOutputsBase+`build_outputs: ["bin/**", "out/"]`+"\n")
	if got := spec.BuildOutputs; len(got) != 2 || got[0] != "bin/**" || got[1] != "out/" {
		t.Fatalf("build_outputs = %q, want [bin/** out/] in order", got)
	}
	if got := mustParse(t, buildOutputsBase).BuildOutputs; got != nil {
		t.Fatalf("absent build_outputs = %q, want nil", got)
	}
	// An empty list is the same as none: no grant, and never folded into
	// `writes`, whose nil means unrestricted.
	empty := mustParse(t, buildOutputsBase+"build_outputs: []\n")
	if empty.BuildOutputs == nil || len(empty.BuildOutputs) != 0 {
		t.Fatalf("build_outputs: [] = %#v, want an empty list", empty.BuildOutputs)
	}
	if writes := empty.Roster["reviewer"].Writes; writes == nil || len(writes) != 0 {
		t.Fatalf("reviewer writes = %#v, want the declared read-only []", writes)
	}
}

func TestBuildOutputsAreValidatedAtSaveTime(t *testing.T) {
	for _, accepted := range []string{"bin/**", "out/", "**/__pycache__/**", "*/bin/**"} {
		t.Run("accepts "+accepted, func(t *testing.T) {
			mustParse(t, buildOutputsBase+fmt.Sprintf("build_outputs: [%q]\n", accepted))
		})
	}
	for _, rejected := range []string{
		"", "   ", "bin\x00/**", "bin\n/**", "/abs/**", "../x/**", "a/../b/**", "-x/**", ":x/**",
		".git/**", "x/.git/**", "**", "*", "**/*", "*/", "bin/*.o", "**/*.o", "bin/*/", "bin",
	} {
		t.Run(fmt.Sprintf("rejects %q", rejected), func(t *testing.T) {
			// JSON string syntax is a valid YAML double-quoted scalar, so
			// NUL and newline survive into the parsed value.
			quoted, err := json.Marshal(rejected)
			if err != nil {
				t.Fatal(err)
			}
			mustReject(t, buildOutputsBase+"build_outputs: ["+string(quoted)+"]\n",
				"build_outputs", fmt.Sprintf("%q", rejected))
		})
	}
}

func TestUnknownYAMLFieldIsRejectedAtParseTime(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
phases:
  - name: test
    kind: code
    command: just check
    on_fial:
      run: build
`, "on_fial")
}

func TestRoleEffortIsParsedWhenInTheVocabulary(t *testing.T) {
	for _, level := range EffortLevels {
		spec := mustParse(t, `
name: effort
roster:
  builder:
    model: opus
    effort: `+level+`
    system_prompt: s
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: builder
acceptance: [all_phases_passed]
`)
		if got := spec.Roster["builder"].Effort; got != level {
			t.Fatalf("effort = %q, want %q", got, level)
		}
	}
}

func TestUnknownRoleEffortIsRejectedNamingRoleAndLevel(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder:
    model: opus
    effort: extreme
    system_prompt: s
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: builder
`, `"builder"`, `"extreme"`, "low, medium, high, xhigh, max")
}

func TestComparisonIfGuardParsesAndMalformedOneIsRejected(t *testing.T) {
	spec := mustParse(t, `
name: guard
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u}
phases:
  - {name: classify, kind: code, command: "echo '{\"risk\":\"high\"}'", reports_fields: true}
  - {name: extra-review, kind: agent, owner: builder, if: "risk == high"}
  - {name: retest, kind: code, command: "true", if: revised}
acceptance: [all_phases_passed]
`)
	if !IsPredicate(spec.Phases[1].If) || IsPredicate(spec.Phases[2].If) {
		t.Fatalf("IsPredicate misclassified the guards %q / %q", spec.Phases[1].If, spec.Phases[2].If)
	}
	mustReject(t, `
name: bad
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder, if: "risk = high"}
`, `"build"`, "if:", `"="`)
}

func TestReportsFieldsIsRejectedOnAgentPhases(t *testing.T) {
	mustReject(t, `
name: bad
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder, reports_fields: true}
`, `"build"`, "reports_fields is for code phases")
}

func TestPublishHoldWhenMustBeAPredicate(t *testing.T) {
	spec := mustParse(t, `
name: hold
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
publish:
  hold_when: "risk == high"
`)
	if spec.Publish == nil || spec.Publish.HoldWhen != "risk == high" {
		t.Fatalf("publish = %+v", spec.Publish)
	}
	mustReject(t, `
name: bad
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
publish:
  hold_when: risky
`, "publish: hold_when", "not of the form")
	mustReject(t, `
name: bad
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
publish: {}
`, "declare hold_when, ci, or both")
}

func TestPublishCIWaitIsOptInWithABoundedTimeout(t *testing.T) {
	const base = `
name: ci
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
`
	if spec := mustParse(t, base); spec.WaitsForCI() {
		t.Fatal("a definition with no publish block waits for CI; CI wait is opt-in")
	}
	if spec := mustParse(t, base+"publish: {hold_when: \"risk == high\"}\n"); spec.WaitsForCI() {
		t.Fatal("a definition with only hold_when waits for CI")
	}
	if spec := mustParse(t, base+"publish: {ci: {wait: false}}\n"); spec.WaitsForCI() {
		t.Fatal("ci.wait: false waits for CI")
	}

	spec := mustParse(t, base+"publish: {ci: {wait: true}}\n")
	if !spec.WaitsForCI() || spec.CITimeout() != DefaultCITimeout {
		t.Fatalf("waits=%v timeout=%s, want a wait with the default %s",
			spec.WaitsForCI(), spec.CITimeout(), DefaultCITimeout)
	}
	spec = mustParse(t, base+"publish: {hold_when: \"risk != low\", ci: {wait: true, timeout: 45m}}\n")
	if !spec.WaitsForCI() || spec.CITimeout() != 45*time.Minute || spec.Publish.HoldWhen != "risk != low" {
		t.Fatalf("publish = %+v timeout=%s, want hold_when and a 45m CI wait", spec.Publish, spec.CITimeout())
	}

	mustReject(t, base+"publish: {ci: {wait: true, timeout: soon}}\n", "publish: ci: timeout", "not a duration")
	mustReject(t, base+"publish: {ci: {wait: true, timeout: 30s}}\n", "publish: ci: timeout", "outside")
	mustReject(t, base+"publish: {ci: {wait: true, timeout: 7h}}\n", "publish: ci: timeout", "outside")
}

func TestPublishCIRepairIsBoundedAndJudgedByTheLaterChain(t *testing.T) {
	const base = `
name: ci-repair
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u}
  reviewer: {model: opus, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
  - {name: test, kind: code, owner: builder, command: "go test ./..."}
  - {name: review, kind: agent, owner: reviewer}
`
	spec := mustParse(t, base+"publish: {ci: {wait: true, on_fail: {run: build, budget: 1}}}\n")
	repair := spec.Publish.CI.OnFail
	if repair == nil || repair.Run != "build" || repair.Budget != 1 {
		t.Fatalf("on_fail = %+v, want run build, budget 1", repair)
	}
	mustParse(t, base+fmt.Sprintf(
		"publish: {ci: {wait: true, on_fail: {run: build, budget: %d}}}\n", MaxCIRepairRounds))
	if spec := mustParse(t, base+"publish: {ci: {wait: true}}\n"); spec.Publish.CI.OnFail != nil {
		t.Fatalf("on_fail = %+v, want nil when undeclared", spec.Publish.CI.OnFail)
	}

	for _, rejected := range []struct {
		name, onFail string
		want         []string
	}{
		{"no wait", "{run: build, budget: 1}", []string{"wait: true"}},
		{"undefined run", "{run: fix, budget: 1}", []string{"run targets undefined phase", `"fix"`}},
		{"code run", "{run: test, budget: 1}", []string{`"test"`, "agent phase"}},
		{"run is last", "{run: review, budget: 1}", []string{`"review"`, "last phase"}},
		{"zero budget", "{run: build}", []string{"budget 0", "outside"}},
		{"budget over cap", fmt.Sprintf("{run: build, budget: %d}", MaxCIRepairRounds+1), []string{"outside"}},
	} {
		t.Run(rejected.name, func(t *testing.T) {
			wait := "true"
			if rejected.name == "no wait" {
				wait = "false"
			}
			mustReject(t, base+fmt.Sprintf("publish: {ci: {wait: %s, on_fail: %s}}\n", wait, rejected.onFail),
				append([]string{"publish: ci: on_fail"}, rejected.want...)...)
		})
	}
	// Skipping a phase between the fix and the end is not expressible: the
	// field that would have allowed it does not exist.
	mustReject(t, base+"publish: {ci: {wait: true, on_fail: {run: build, resume_from: review, budget: 1}}}\n",
		"resume_from")
}

func TestPublishCIRerunIsBoundedAndNeedsAWait(t *testing.T) {
	const base = `
name: ci-rerun
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
`
	if spec := mustParse(t, base+"publish: {ci: {wait: true}}\n"); spec.Publish.CI.Rerun != nil {
		t.Fatalf("rerun = %+v, want nil when undeclared", spec.Publish.CI.Rerun)
	}
	for budget := 1; budget <= MaxCIReruns; budget++ {
		spec := mustParse(t, base+fmt.Sprintf("publish: {ci: {wait: true, rerun: {budget: %d}}}\n", budget))
		if spec.Publish.CI.Rerun == nil || spec.Publish.CI.Rerun.Budget != budget {
			t.Fatalf("rerun = %+v, want budget %d", spec.Publish.CI.Rerun, budget)
		}
	}
	if MaxCIReruns != 3 {
		t.Fatalf("MaxCIReruns = %d, want 3", MaxCIReruns)
	}
	mustReject(t, base+"publish: {ci: {wait: false, rerun: {budget: 1}}}\n",
		"publish: ci: rerun", "wait: true")
	mustReject(t, base+"publish: {ci: {rerun: {budget: 1}}}\n",
		"publish: ci: rerun", "wait: true")
	mustReject(t, base+"publish: {ci: {wait: true, rerun: {}}}\n",
		"publish: ci: rerun: budget 0", "outside 1..3")
	mustReject(t, base+"publish: {ci: {wait: true, rerun: {budget: 4}}}\n",
		"publish: ci: rerun: budget 4", "outside 1..3")
}

func TestNegativeRoleBudgetIsRejected(t *testing.T) {
	spec := mustParse(t, `
name: budget
roster:
  builder: {model: opus, budget_usd: 2.5, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
`)
	if spec.Roster["builder"].BudgetUSD != 2.5 {
		t.Fatalf("budget_usd = %v", spec.Roster["builder"].BudgetUSD)
	}
	mustReject(t, `
name: bad
roster:
  builder: {model: opus, budget_usd: -1, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
`, `"builder"`, "budget_usd must not be negative")
}

func TestRuntimeIsOptionalAndParsedWhenInTheVocabulary(t *testing.T) {
	if spec := mustParse(t, simpleSDLC); spec.Runtime != "" {
		t.Fatalf("runtime = %q, want empty (any worker)", spec.Runtime)
	}
	for _, runtime := range Runtimes {
		spec := mustParse(t, `
name: pinned
runtime: `+runtime+`
roster:
  builder: {model: m, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
`)
		if spec.Runtime != runtime {
			t.Fatalf("runtime = %q, want %q", spec.Runtime, runtime)
		}
	}
}

func TestUnknownRuntimeIsRejectedNamingTheFieldAndAllowedValues(t *testing.T) {
	mustReject(t, `
name: bad
runtime: pi
roster:
  builder: {model: m, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
`, `runtime "pi"`, "claude-code, codex")
}

func TestCodexRuntimeRejectsARoleWithABudgetOrToolAllowlist(t *testing.T) {
	mustReject(t, `
name: bad
runtime: codex
roster:
  builder: {model: m, budget_usd: 2.5, system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
`, `role "builder"`, "budget_usd cannot be enforced")
	mustReject(t, `
name: bad
runtime: codex
roster:
  builder: {model: m, tools: [read], system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
`, `role "builder"`, "tools: list cannot be enforced")
	mustParse(t, `
name: fine
runtime: claude-code
roster:
  builder: {model: m, budget_usd: 2.5, tools: [read], system_prompt: s, user_prompt: u}
phases:
  - {name: build, kind: agent, owner: builder}
`)
}

// parallelPanel is a builder, three read-only reviewers, and a code check;
// the tests below splice roster entries, phases, and group lines into it.
func parallelPanel(extraRoster, phases, tail string) string {
	return `
name: panel
roster:
  builder: {model: opus, system_prompt: s, user_prompt: u, writes: ["src/"]}
  a: {model: opus, system_prompt: s, user_prompt: u, writes: []}
  b: {model: opus, system_prompt: s, user_prompt: u, writes: []}
  c: {model: opus, system_prompt: s, user_prompt: u, writes: []}
` + extraRoster + `
phases:
` + phases + tail
}

const panelPhases = `
  - {name: build, kind: agent, owner: builder}
  - {name: review-a, kind: agent, owner: a, on_fail: {when: "approved == false", run: build, then: rerun-self, budget: 1}}
  - {name: review-b, kind: agent, owner: b}
  - {name: review-c, kind: agent, owner: c}
  - {name: check, kind: code, command: "true"}
`

func TestAParallelGroupOfConsecutiveReadOnlyReviewersValidates(t *testing.T) {
	spec := mustParse(t, parallelPanel("", panelPhases, "parallel: [review-a, review-b, review-c]\n"))
	start, end, ok := spec.ParallelRange()
	if !ok || start != 1 || end != 4 {
		t.Fatalf("ParallelRange = %d, %d, %v, want 1, 4, true", start, end, ok)
	}
	if _, _, ok := mustParse(t, parallelPanel("", panelPhases, "")).ParallelRange(); ok {
		t.Fatal("a definition without a group reports one")
	}
	// Declared build outputs are the one thing members may write: a group
	// beside them still validates.
	spec = mustParse(t, parallelPanel("", panelPhases,
		"parallel: [review-a, review-b, review-c]\nbuild_outputs: [\"bin/**\"]\n"))
	if _, _, ok := spec.ParallelRange(); !ok || len(spec.BuildOutputs) != 1 {
		t.Fatalf("group or build_outputs lost: parallel=%v build_outputs=%v", spec.Parallel, spec.BuildOutputs)
	}
}

// A parallel panel and the CI re-run and repair policies are independent:
// declared together they validate, and each keeps its own rules.
func TestAParallelGroupValidatesWithCIRerunsAndRepair(t *testing.T) {
	const publish = "publish: {ci: {wait: true, rerun: {budget: 2}, on_fail: {run: build, budget: 1}}}\n"
	spec := mustParse(t, parallelPanel("", panelPhases, "parallel: [review-a, review-b, review-c]\n"+publish))
	if _, _, ok := spec.ParallelRange(); !ok {
		t.Fatal("the group was lost next to a publish block")
	}
	if spec.Publish.CI.Rerun == nil || spec.Publish.CI.Rerun.Budget != 2 || spec.Publish.CI.OnFail == nil {
		t.Fatalf("publish.ci = %+v, want the re-run and repair policies kept", spec.Publish.CI)
	}
	mustReject(t, parallelPanel("", panelPhases,
		"parallel: [review-a, review-b, review-c]\npublish: {ci: {wait: true, rerun: {budget: 4}}}\n"),
		"publish: ci: rerun: budget 4")
	mustReject(t, parallelPanel("", panelPhases,
		"parallel: [build, review-a]\n"+publish), "parallel")
}

// R8: every rule that keeps a group's members concurrent-safe is enforced at
// save time, naming what broke it.
func TestAParallelGroupIsRejectedUnlessItsMembersAreConcurrentSafe(t *testing.T) {
	unrestricted := "  d: {model: opus, system_prompt: s, user_prompt: u}\n"
	cases := []struct {
		name   string
		source string
		want   []string
	}{
		{"a writing member", parallelPanel("", panelPhases, "parallel: [build, review-a]\n"),
			[]string{`"build"`, `"builder"`, "writes: []"}},
		{"a writing member beside declared build outputs", parallelPanel("", panelPhases,
			"parallel: [build, review-a]\nbuild_outputs: [\"bin/**\"]\n"),
			[]string{`"build"`, `"builder"`, "writes: []", "build_outputs"}},
		{"a member whose role omits writes, which is unrestricted", parallelPanel(unrestricted,
			panelPhases+"  - {name: review-d, kind: agent, owner: d}\n  - {name: review-e, kind: agent, owner: a}\n",
			"parallel: [review-d, review-e]\n"),
			[]string{`"review-d"`, `"d"`, "writes: []"}},
		{"a code-phase member", parallelPanel("", panelPhases, "parallel: [review-c, check]\n"),
			[]string{`"check"`, "code phase"}},
		{"two members sharing a role", parallelPanel("",
			panelPhases+"  - {name: review-a2, kind: agent, owner: a}\n  - {name: review-a3, kind: agent, owner: a}\n",
			"parallel: [review-a2, review-a3]\n"),
			[]string{`"review-a2"`, `"review-a3"`, `"a"`, "own role"}},
		{"non-consecutive members", parallelPanel("", panelPhases, "parallel: [review-a, review-c]\n"),
			[]string{"consecutive", `"review-c"`, `"review-a"`}},
		{"members out of chain order", parallelPanel("", panelPhases, "parallel: [review-b, review-a]\n"),
			[]string{"consecutive"}},
		{"a member guarded on a sibling's repair field", parallelPanel("",
			strings.Replace(panelPhases, "owner: b}", `owner: b, if: "approved == true"}`, 1),
			"parallel: [review-a, review-b]\n"),
			[]string{`"review-b"`, `"approved"`, `"review-a"`}},
		{"a member guarded on a sibling's base envelope field", parallelPanel("",
			strings.Replace(panelPhases, "owner: c}", "owner: c, if: summary}", 1),
			"parallel: [review-b, review-c]\n"),
			[]string{`"review-c"`, `"summary"`}},
		{"a second group", parallelPanel("", panelPhases, "parallel: [[review-a, review-b], [review-c]]\n"),
			[]string{"only one parallel group"}},
		{"a second group key", parallelPanel("", panelPhases,
			"parallel: [review-a, review-b]\nparallel: [review-c]\n"),
			[]string{"parallel"}},
		{"a group of one", parallelPanel("", panelPhases, "parallel: [review-a]\n"),
			[]string{"two or more"}},
		{"an undefined member", parallelPanel("", panelPhases, "parallel: [review-a, review-z]\n"),
			[]string{`"review-z"`}},
		{"a member listed twice", parallelPanel("", panelPhases, "parallel: [review-a, review-a]\n"),
			[]string{"twice"}},
		{"CI repair starting inside the group", parallelPanel("", panelPhases,
			"parallel: [review-a, review-b]\npublish: {ci: {wait: true, on_fail: {run: review-a, budget: 1}}}\n"),
			[]string{"on_fail", `"review-a"`, "parallel group member"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustReject(t, tc.source, tc.want...)
		})
	}
	// A guard on a field no sibling declares is fine: it is judged against
	// the view that preceded the group, exactly as the definition says.
	mustParse(t, parallelPanel("",
		strings.Replace(panelPhases, "owner: c}", `owner: c, if: "risk != low"}`, 1),
		"parallel: [review-a, review-b, review-c]\n"))
}
