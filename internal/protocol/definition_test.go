package protocol

import (
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
