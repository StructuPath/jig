package protocol

import (
	"bytes"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Built-in gate registry (R9). Gates verify envelope claims after a phase
// completes; anything a repo needs beyond this registry is a code phase, not
// a new gate.
const (
	GateArtifactsExist    = "artifacts_exist"
	GateFilesNonEmpty     = "files_non_empty"
	GateDiffMatchesClaims = "diff_matches_claims"
	GateVerdictConsistent = "verdict_consistent"
	GateTestsPass         = "tests_pass"
	GateTestsIntact       = "tests_intact"
)

// CheckAllPhasesPassed is the built-in acceptance-predicate check that every
// phase in the chain reached success. The other resolvable acceptance checks
// are references to gates declared in the chain (R1, R12).
const CheckAllPhasesPassed = "all_phases_passed"

var builtinGates = map[string]bool{
	GateArtifactsExist:    true,
	GateFilesNonEmpty:     true,
	GateDiffMatchesClaims: true,
	GateVerdictConsistent: true,
	GateTestsPass:         true,
	GateTestsIntact:       true,
}

// Repair-edge continuation and exhaustion policies (KTD2).
const (
	RepairThenRerunSelf  = "rerun-self"
	RepairThenRerunChain = "rerun-chain"

	RepairExhaustedFailJob = "fail-job"
	RepairExhaustedProceed = "proceed"
)

// EffortLevels is the accepted `effort:` vocabulary, lowest to highest. It is
// Claude Code's `--effort` set; the Codex adapter maps it onto that CLI's
// reasoning-effort setting.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

var effortLevels = func() map[string]bool {
	levels := make(map[string]bool, len(EffortLevels))
	for _, level := range EffortLevels {
		levels[level] = true
	}
	return levels
}()

// Runtime names a definition may pin with `runtime:`. They are the names
// workers advertise in their registration's runtime capabilities, so claim
// eligibility compares them as plain strings (R17, U10).
const (
	RuntimeClaudeCode = "claude-code"
	RuntimeCodex      = "codex"
)

// Runtimes is the accepted `runtime:` vocabulary.
var Runtimes = []string{RuntimeClaudeCode, RuntimeCodex}

// DefinitionSpec is the parsed YAML form of a Job Definition (R1, KTD2): an
// ordered phase chain, a per-role roster, and an acceptance predicate of
// named checks. Validate enforces the save-time contract; nothing downstream
// re-checks it. Runtime, when set, restricts the job to workers that
// advertise that runtime; empty means any worker, which is how every
// snapshot frozen before the field existed keeps claiming.
type DefinitionSpec struct {
	Name       string              `yaml:"name"`
	Runtime    string              `yaml:"runtime"`
	Roster     map[string]RoleSpec `yaml:"roster"`
	Phases     []PhaseSpec         `yaml:"phases"`
	Parallel   ParallelGroup       `yaml:"parallel"`
	Acceptance []string            `yaml:"acceptance"`
	Publish    *PublishSpec        `yaml:"publish"`

	// BuildOutputs names the directories any role — `writes: []` reviewers
	// and parallel-group members included — may write without breaching the
	// write boundary, so a read-only role can run the repository's own check
	// command (`go build -o bin/...`). Every entry is a directory grant:
	// `<dir>/**` (glob characters allowed before the `/**`) or a literal
	// `<dir>/`. A path is a build output only while it is NOT tracked in
	// HEAD; tracked files and git metadata are never covered.
	//
	// Scope: the grant governs how the write boundary classifies untracked
	// paths and, through it, the engine-computed changed paths jig stages
	// at publish — an output is never one of them, so jig never stages it.
	// It does not inspect commits: publish pushes HEAD as it stands, so an
	// output a role commits itself is published under that role's `writes`
	// authority, and thereafter it is tracked repository content. nil and
	// an empty list are the same: no grant. It is never merged into `writes`.
	BuildOutputs []string `yaml:"build_outputs"`
}

// ParallelGroup is the one opt-in parallel construct: the names of two or
// more consecutive agent phases, in chain order, that run concurrently as
// one step of the chain. Every member is a read-only role (`writes: []`,
// apart from the definition's declared `build_outputs`) with its own owner,
// and no member's `if:` guard reads a field a sibling reports, because
// every member is judged against the envelope and field view that preceded
// the group. Members' results merge in declared order, and the phase after
// the group receives the last member's envelope. Build outputs written
// during the group survive it whatever happened to the member that wrote
// them: the group takes one snapshot and enforces once, so there is no
// per-member attribution and no per-member rollback.
//
// It reopens v1's "no parallel phases" non-goal (V1:KTD2) for exactly this
// shape and no other: reviewers that read the same tree and report
// verdicts. One group per definition.
type ParallelGroup []string

// UnmarshalYAML accepts a flat list of phase names. A list of lists is a
// second group, which is refused by name rather than as a type error.
func (group *ParallelGroup) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("parallel: line %d: must be a list of phase names", node.Line)
	}
	names := make(ParallelGroup, 0, len(node.Content))
	for _, item := range node.Content {
		if item.Kind == yaml.SequenceNode {
			return fmt.Errorf("parallel: line %d: only one parallel group per definition — "+
				"declare it as a flat list of phase names", item.Line)
		}
		if item.Kind != yaml.ScalarNode {
			return fmt.Errorf("parallel: line %d: a member must be a phase name", item.Line)
		}
		names = append(names, item.Value)
	}
	*group = names
	return nil
}

// ParallelRange locates the group in the chain: Phases[start:end] are its
// members. ok is false when the definition declares no group. Call it only
// on a spec that passed Validate.
func (spec *DefinitionSpec) ParallelRange() (start, end int, ok bool) {
	if len(spec.Parallel) == 0 {
		return 0, 0, false
	}
	for i, phase := range spec.Phases {
		if phase.Name == spec.Parallel[0] {
			return i, i + len(spec.Parallel), true
		}
	}
	return 0, 0, false
}

// envelopeBaseFields are reported by every agent phase: the base contract.
var envelopeBaseFields = []string{"status", "summary", "artifacts", "notes_for_next_agent"}

// PublishSpec is the definition's say over delivery. HoldWhen is a declared
// envelope predicate (the `on_fail.when` language) evaluated against the
// chain's merged field view once acceptance has passed: when it holds, the
// attempt ends accepted_unpublished with a recorded hold instead of a branch
// and pull request, and the operator's publish retry is the human review
// step that releases it. A change a classifier scored `risk == high` waits
// for a person; everything else ships. CI, when it waits, makes a green CI
// run on the pull request's head part of what `accepted` means.
type PublishSpec struct {
	HoldWhen string  `yaml:"hold_when"`
	CI       *CISpec `yaml:"ci"`
}

// CISpec opts a definition into waiting for the pull request's CI after
// publish. Timeout is a Go duration ("30m"); empty means DefaultCITimeout.
// OnFail, when declared, repairs red CI inside the attempt instead of ending
// it there. Rerun, when declared, re-runs failed GitHub Actions jobs on the
// same head before any repair round, to tell a flaky check from a real one.
type CISpec struct {
	Wait    bool          `yaml:"wait"`
	Timeout string        `yaml:"timeout"`
	OnFail  *CIRepairSpec `yaml:"on_fail"`
	Rerun   *CIRerunSpec  `yaml:"rerun"`
}

// CIRerunSpec is the flaky-check re-run policy:
//
//	rerun: {budget: N}
//
// Budget counts re-runs per attempt, not per head: a repair round's new head
// gets only what the earlier heads left. A re-run never changes the branch,
// and a pass after one is reported as flaky, never as a clean pass.
type CIRerunSpec struct {
	Budget int `yaml:"budget"`
}

// CIRepairSpec is the CI repair loop:
//
//	on_fail: {run: <agent phase>, budget: N}
//
// A red CI run is handed to Run as its input envelope; every phase after Run
// then runs again, gates and repair edges live, before the fix may be pushed
// and CI awaited again. There is deliberately no way to resume later in the
// chain: a skipped phase would carry its verdict on the old code into
// acceptance of the new. Budget bounds the rounds.
type CIRepairSpec struct {
	Run    string `yaml:"run"`
	Budget int    `yaml:"budget"`
}

// CI wait bounds. The timeout covers the whole wait for one head commit; the
// ceiling keeps a typo like "30h" from parking a worker slot for a day.
const (
	DefaultCITimeout = 30 * time.Minute
	MinCITimeout     = time.Minute
	MaxCITimeout     = 6 * time.Hour

	// MaxCIRepairRounds caps publish.ci.on_fail's budget. Each round re-runs
	// every phase after the repair phase, reviewers included, so a
	// larger budget is mostly a larger bill for a fix that is not converging.
	MaxCIRepairRounds = 3

	// MaxCIReruns caps publish.ci.rerun's budget. Re-running more often than
	// this stops telling a flaky check from a broken one and starts hiding it.
	MaxCIReruns = 3
)

// WaitsForCI reports whether the definition gates acceptance on CI.
func (spec *DefinitionSpec) WaitsForCI() bool {
	return spec.Publish != nil && spec.Publish.CI != nil && spec.Publish.CI.Wait
}

// CITimeout is the validated CI wait budget, DefaultCITimeout when unset.
// Call it only on a spec that passed Validate.
func (spec *DefinitionSpec) CITimeout() time.Duration {
	if !spec.WaitsForCI() || strings.TrimSpace(spec.Publish.CI.Timeout) == "" {
		return DefaultCITimeout
	}
	timeout, err := time.ParseDuration(spec.Publish.CI.Timeout)
	if err != nil {
		return DefaultCITimeout
	}
	return timeout
}

// RoleSpec is one roster entry: the model, prompts, and allowlists an agent
// role runs with. Prompts are content or a repo-relative path, exclusively.
// Env is the per-role environment allowlist (KTD10); SensitiveEnv names the
// subset whose values are redacted from every persisted trace (R15).
// Effort, when set, is the per-role effort level handed to the runtime CLI;
// empty leaves the CLI's own default for the model. BudgetUSD, when
// positive, caps what one send by this role may spend; the runtime CLI
// enforces it, so a runtime with no such control refuses the send rather
// than run uncapped.
type RoleSpec struct {
	Model            string   `yaml:"model"`
	Effort           string   `yaml:"effort"`
	BudgetUSD        float64  `yaml:"budget_usd"`
	Thinking         string   `yaml:"thinking"`
	SystemPrompt     string   `yaml:"system_prompt"`
	SystemPromptPath string   `yaml:"system_prompt_path"`
	UserPrompt       string   `yaml:"user_prompt"`
	UserPromptPath   string   `yaml:"user_prompt_path"`
	Tools            []string `yaml:"tools"`
	Writes           []string `yaml:"writes"`
	Env              []string `yaml:"env"`
	SensitiveEnv     []string `yaml:"sensitive_env"`
}

// PhaseSpec is one link of the chain. Agent phases have an Owner role from
// the roster; code phases have a Command (and Owner only if they need a
// role's environment). If guards the phase (KTD2): a bare field name runs
// the phase only when a previous envelope set that field truthy; a
// `<field> == <literal>` predicate runs it only when the comparison holds.
// ReportsFields, on a code phase, makes the command's last stdout line a
// JSON object of envelope fields — the way a deterministic script (a risk
// classifier, say) speaks to `if:` guards and the publish hold.
type PhaseSpec struct {
	Name          string      `yaml:"name"`
	Kind          string      `yaml:"kind"`
	Owner         string      `yaml:"owner"`
	Command       string      `yaml:"command"`
	If            string      `yaml:"if"`
	ReportsFields bool        `yaml:"reports_fields"`
	Gates         []GateSpec  `yaml:"gates"`
	OnFail        *RepairEdge `yaml:"on_fail"`
}

// GateSpec configures one built-in gate on a phase. Budget is the
// gate-correction retry budget; zero means DefaultPhaseRetryBudget. Command
// is required by tests_pass and meaningless to every other gate. Allow is
// tests_intact's list of path.Match globs naming test files a definition
// authorizes the agent to delete or weaken; it is rejected on any other gate.
type GateSpec struct {
	Name    string   `yaml:"name"`
	Command string   `yaml:"command"`
	Budget  int      `yaml:"budget"`
	Allow   []string `yaml:"allow"`
}

// RepairEdge is the one declared loop construct (KTD2):
//
//	on_fail: {run: <phase>, then: rerun-self, budget: N, exhausted: fail-job|proceed}
//
// Failure that triggers the edge is nonzero exit for code phases and the
// declared When envelope predicate for agent phases. Budget bounds the loop;
// an edge without a positive budget is rejected at save time.
// then: rerun-chain runs the target and every intervening phase before
// retesting the failed phase; its target must precede the failed phase.
type RepairEdge struct {
	When      string `yaml:"when"`
	Run       string `yaml:"run"`
	Then      string `yaml:"then"`
	Budget    int    `yaml:"budget"`
	Exhausted string `yaml:"exhausted"`
}

// EnvelopePredicate is a declared comparison against one envelope field,
// parsed from the form `<field> == <literal>` or `<field> != <literal>`
// (e.g. `approved == false`). It is the only predicate language definitions
// may use; anything richer is a code phase.
type EnvelopePredicate struct {
	Field   string
	Op      string
	Literal string
}

// IsPredicate reports whether an `if:` guard is a comparison rather than a
// bare field name: anything with more than one whitespace-separated token
// must parse as a predicate, so a typo like `risk = high` is a save-time
// error rather than a guard on a field literally named "risk = high".
func IsPredicate(guard string) bool { return len(strings.Fields(guard)) > 1 }

// ParsePredicate parses a declared envelope predicate, rejecting anything
// that is not exactly `<field> <==|!=> <literal>`.
func ParsePredicate(expression string) (EnvelopePredicate, error) {
	fields := strings.Fields(expression)
	if len(fields) != 3 {
		return EnvelopePredicate{}, fmt.Errorf(
			"predicate %q is not of the form <field> == <literal>", expression)
	}
	if fields[1] != "==" && fields[1] != "!=" {
		return EnvelopePredicate{}, fmt.Errorf(
			"predicate %q: operator %q is not \"==\" or \"!=\"", expression, fields[1])
	}
	return EnvelopePredicate{
		Field:   fields[0],
		Op:      fields[1],
		Literal: strings.Trim(fields[2], `"'`),
	}, nil
}

// ParseDefinition decodes definition YAML strictly — unknown fields are
// rejected so a typo like `on_fial` fails at save time instead of silently
// dropping a repair edge — and then validates it.
func ParseDefinition(source []byte) (*DefinitionSpec, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	var spec DefinitionSpec
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("parse definition: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// Validate enforces the save-time contract (R1, KTD2). Every rejection names
// the offending element: the operator fixes a definition from the message
// alone, without reading this code.
func (spec *DefinitionSpec) Validate() error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("definition: name is required")
	}
	if len(spec.Phases) == 0 {
		return fmt.Errorf("definition %q: at least one phase is required", spec.Name)
	}
	for role, entry := range spec.Roster {
		if err := entry.validate(role); err != nil {
			return err
		}
	}
	if err := spec.validateRuntime(); err != nil {
		return err
	}
	if err := spec.validateBuildOutputs(); err != nil {
		return err
	}
	phasesByName := make(map[string]PhaseSpec, len(spec.Phases))
	for _, phase := range spec.Phases {
		if strings.TrimSpace(phase.Name) == "" {
			return fmt.Errorf("definition %q: every phase requires a name", spec.Name)
		}
		if _, duplicate := phasesByName[phase.Name]; duplicate {
			return fmt.Errorf("phase %q: name is declared twice", phase.Name)
		}
		phasesByName[phase.Name] = phase
	}
	for _, phase := range spec.Phases {
		if err := spec.validatePhase(phase, phasesByName); err != nil {
			return err
		}
	}
	if err := spec.validateRepairAcyclic(phasesByName); err != nil {
		return err
	}
	if err := spec.validateAcceptance(); err != nil {
		return err
	}
	if err := spec.validateParallel(phasesByName); err != nil {
		return err
	}
	return spec.validatePublish(phasesByName)
}

// validateRuntime refuses, at save time, a runtime no worker advertises and
// a Codex definition whose roster asks for what the Codex adapter refuses at
// send time. Caught there instead, that refusal costs every agent start in
// the attempt and fails the job (U10).
func (spec *DefinitionSpec) validateRuntime() error {
	switch spec.Runtime {
	case "", RuntimeClaudeCode:
		return nil
	case RuntimeCodex:
	default:
		return fmt.Errorf("definition %q: runtime %q is not one of %s",
			spec.Name, spec.Runtime, strings.Join(Runtimes, ", "))
	}
	roles := make([]string, 0, len(spec.Roster))
	for role := range spec.Roster {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		entry := spec.Roster[role]
		if len(entry.Tools) > 0 {
			return fmt.Errorf("role %q: runtime codex: codex exec has no tool allowlist flag; "+
				"a role tools: list cannot be enforced", role)
		}
		if entry.BudgetUSD > 0 {
			return fmt.Errorf("role %q: runtime codex: codex exec has no spend-cap flag; "+
				"a role budget_usd cannot be enforced", role)
		}
	}
	return nil
}

// validateParallel enforces R8. What a member "reports" is what the
// definition can see it report: the envelope base fields every agent phase
// emits and the field its own repair edge's `when` reads. An agent envelope
// may carry more, which no save-time check can know.
func (spec *DefinitionSpec) validateParallel(phases map[string]PhaseSpec) error {
	group := spec.Parallel
	if group == nil {
		return nil
	}
	if len(group) < 2 {
		return fmt.Errorf("parallel: a group needs two or more phases, got %d", len(group))
	}
	owners := make(map[string]string, len(group))
	seen := make(map[string]bool, len(group))
	for _, name := range group {
		phase, defined := phases[name]
		if !defined {
			return fmt.Errorf("parallel: member %q is not a phase in the chain", name)
		}
		if seen[name] {
			return fmt.Errorf("parallel: member %q is listed twice", name)
		}
		seen[name] = true
		if phase.Kind != PhaseKindAgent {
			return fmt.Errorf("parallel: member %q is a %s phase; only agent phases run in a group",
				name, phase.Kind)
		}
		if writes := spec.Roster[phase.Owner].Writes; writes == nil || len(writes) > 0 {
			return fmt.Errorf("parallel: member %q runs role %q, which may write; "+
				"every member's role must declare writes: [] (apart from declared build_outputs, "+
				"members are read-only)", name, phase.Owner)
		}
		if other, shared := owners[phase.Owner]; shared {
			return fmt.Errorf("parallel: members %q and %q share owner role %q; "+
				"every member needs its own role", other, name, phase.Owner)
		}
		owners[phase.Owner] = name
	}
	start, _, _ := spec.ParallelRange()
	for i, name := range group {
		if i > 0 && (start+i >= len(spec.Phases) || spec.Phases[start+i].Name != name) {
			return fmt.Errorf("parallel: members must be consecutive phases listed in chain order; "+
				"%q does not directly follow %q in the chain", name, group[i-1])
		}
	}
	for _, name := range group {
		guard := strings.TrimSpace(phases[name].If)
		if guard == "" {
			continue
		}
		field := guard
		if IsPredicate(guard) {
			predicate, _ := ParsePredicate(guard)
			field = predicate.Field
		}
		for _, sibling := range group {
			if sibling != name && reportsField(phases[sibling], field) {
				return fmt.Errorf("parallel: member %q's if: guard reads %q, which member %q reports; "+
					"every member is judged before any member runs", name, field, sibling)
			}
		}
	}
	if spec.Publish != nil && spec.Publish.CI != nil && spec.Publish.CI.OnFail != nil &&
		seen[spec.Publish.CI.OnFail.Run] {
		return fmt.Errorf("publish: ci: on_fail: run phase %q is a parallel group member; "+
			"a CI repair round cannot start inside the group", spec.Publish.CI.OnFail.Run)
	}
	return nil
}

// reportsField reports whether an agent phase declares that it reports the
// field: a base envelope field, or the field its repair edge keys on.
func reportsField(phase PhaseSpec, field string) bool {
	for _, base := range envelopeBaseFields {
		if field == base {
			return true
		}
	}
	if phase.OnFail != nil && phase.OnFail.When != "" {
		if predicate, err := ParsePredicate(phase.OnFail.When); err == nil && predicate.Field == field {
			return true
		}
	}
	return false
}

func (spec *DefinitionSpec) validatePublish(phases map[string]PhaseSpec) error {
	if spec.Publish == nil {
		return nil
	}
	hold := strings.TrimSpace(spec.Publish.HoldWhen)
	if hold == "" && spec.Publish.CI == nil {
		return fmt.Errorf("publish: declare hold_when, ci, or both")
	}
	if hold != "" {
		if _, err := ParsePredicate(spec.Publish.HoldWhen); err != nil {
			return fmt.Errorf("publish: hold_when: %w", err)
		}
	}
	if ci := spec.Publish.CI; ci != nil && strings.TrimSpace(ci.Timeout) != "" {
		timeout, err := time.ParseDuration(ci.Timeout)
		if err != nil {
			return fmt.Errorf("publish: ci: timeout %q is not a duration like \"30m\"", ci.Timeout)
		}
		if timeout < MinCITimeout || timeout > MaxCITimeout {
			return fmt.Errorf("publish: ci: timeout %s is outside %s..%s",
				timeout, MinCITimeout, MaxCITimeout)
		}
	}
	if err := spec.validateCIRerun(); err != nil {
		return err
	}
	return spec.validateCIRepair(phases)
}

func (spec *DefinitionSpec) validateCIRerun() error {
	ci := spec.Publish.CI
	if ci == nil || ci.Rerun == nil {
		return nil
	}
	if !ci.Wait {
		return fmt.Errorf("publish: ci: rerun re-runs red CI checks, which needs wait: true")
	}
	if ci.Rerun.Budget < 1 || ci.Rerun.Budget > MaxCIReruns {
		return fmt.Errorf("publish: ci: rerun: budget %d is outside 1..%d", ci.Rerun.Budget, MaxCIReruns)
	}
	return nil
}

func (spec *DefinitionSpec) validateCIRepair(phases map[string]PhaseSpec) error {
	ci := spec.Publish.CI
	if ci == nil || ci.OnFail == nil {
		return nil
	}
	repair := ci.OnFail
	if !ci.Wait {
		return fmt.Errorf("publish: ci: on_fail repairs red CI, which needs wait: true")
	}
	run, defined := phases[repair.Run]
	if !defined {
		return fmt.Errorf("publish: ci: on_fail: run targets undefined phase %q", repair.Run)
	}
	if run.Kind != PhaseKindAgent {
		return fmt.Errorf("publish: ci: on_fail: run phase %q is a %s phase; only an agent phase can fix code",
			repair.Run, run.Kind)
	}
	if spec.Phases[len(spec.Phases)-1].Name == repair.Run {
		return fmt.Errorf(
			"publish: ci: on_fail: run phase %q is the last phase, so nothing after it would judge the fix",
			repair.Run)
	}
	if repair.Budget < 1 || repair.Budget > MaxCIRepairRounds {
		return fmt.Errorf("publish: ci: on_fail: budget %d is outside 1..%d", repair.Budget, MaxCIRepairRounds)
	}
	return nil
}

func (role RoleSpec) validate(name string) error {
	if strings.TrimSpace(role.Model) == "" {
		return fmt.Errorf("role %q: model is required", name)
	}
	if role.Effort != "" && !effortLevels[role.Effort] {
		return fmt.Errorf("role %q: effort %q is not one of %s",
			name, role.Effort, strings.Join(EffortLevels, ", "))
	}
	if role.BudgetUSD < 0 {
		return fmt.Errorf("role %q: budget_usd must not be negative", name)
	}
	if role.SystemPrompt == "" && role.SystemPromptPath == "" {
		return fmt.Errorf(
			"role %q: missing system prompt content — set system_prompt or system_prompt_path", name)
	}
	if role.SystemPrompt != "" && role.SystemPromptPath != "" {
		return fmt.Errorf("role %q: system_prompt and system_prompt_path are mutually exclusive", name)
	}
	if role.UserPrompt == "" && role.UserPromptPath == "" {
		return fmt.Errorf(
			"role %q: missing user prompt content — set user_prompt or user_prompt_path", name)
	}
	if role.UserPrompt != "" && role.UserPromptPath != "" {
		return fmt.Errorf("role %q: user_prompt and user_prompt_path are mutually exclusive", name)
	}
	return nil
}

func (spec *DefinitionSpec) validatePhase(phase PhaseSpec, phases map[string]PhaseSpec) error {
	switch phase.Kind {
	case PhaseKindAgent:
		if phase.Owner == "" {
			return fmt.Errorf("phase %q: agent phases require an owner role", phase.Name)
		}
		if phase.Command != "" {
			return fmt.Errorf("phase %q: agent phases do not take a command", phase.Name)
		}
		if phase.ReportsFields {
			return fmt.Errorf(
				"phase %q: reports_fields is for code phases — an agent phase reports through its envelope",
				phase.Name)
		}
	case PhaseKindCode:
		if strings.TrimSpace(phase.Command) == "" {
			return fmt.Errorf("phase %q: code phases require a command", phase.Name)
		}
	default:
		return fmt.Errorf("phase %q: kind %q is not %q or %q",
			phase.Name, phase.Kind, PhaseKindAgent, PhaseKindCode)
	}
	if IsPredicate(phase.If) {
		if _, err := ParsePredicate(phase.If); err != nil {
			return fmt.Errorf("phase %q: if: %w", phase.Name, err)
		}
	}
	if phase.Owner != "" {
		if _, defined := spec.Roster[phase.Owner]; !defined {
			return fmt.Errorf("phase %q: owner role %q is not defined in the roster",
				phase.Name, phase.Owner)
		}
	}
	for _, gate := range phase.Gates {
		if !builtinGates[gate.Name] {
			return fmt.Errorf("phase %q: gate %q is not in the built-in registry (%s)",
				phase.Name, gate.Name, strings.Join(builtinGateNames(), ", "))
		}
		if gate.Name == GateTestsPass && strings.TrimSpace(gate.Command) == "" {
			return fmt.Errorf("phase %q: gate %q requires a command", phase.Name, GateTestsPass)
		}
		if gate.Budget < 0 {
			return fmt.Errorf("phase %q: gate %q budget must not be negative", phase.Name, gate.Name)
		}
		if err := validateGateAllow(gate); err != nil {
			return fmt.Errorf("phase %q: gate %q: %w", phase.Name, gate.Name, err)
		}
	}
	return spec.validateRepairEdge(phase, phases)
}

func (spec *DefinitionSpec) validateRepairEdge(phase PhaseSpec, phases map[string]PhaseSpec) error {
	edge := phase.OnFail
	if edge == nil {
		return nil
	}
	if _, defined := phases[edge.Run]; !defined {
		return fmt.Errorf("phase %q: repair edge targets undefined phase %q", phase.Name, edge.Run)
	}
	if edge.Run == phase.Name {
		return fmt.Errorf(
			"phase %q: repair edge targets itself — rerun-self already reruns the failed phase",
			phase.Name)
	}
	if edge.Then != RepairThenRerunSelf && edge.Then != RepairThenRerunChain {
		return fmt.Errorf("phase %q: repair edge \"then\" must be %q or %q, got %q",
			phase.Name, RepairThenRerunSelf, RepairThenRerunChain, edge.Then)
	}
	if edge.Then == RepairThenRerunChain {
		found := false
		for _, candidate := range spec.Phases {
			if candidate.Name == phase.Name {
				break
			}
			if candidate.Name == edge.Run {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("phase %q: rerun-chain target %q must precede the failed phase", phase.Name, edge.Run)
		}
		// A chain replay runs phases one after another; replaying a parallel
		// group that way would run its members outside the group's read-only
		// boundary.
		inChain := false
		for _, candidate := range spec.Phases {
			if candidate.Name == edge.Run {
				inChain = true
			}
			if inChain {
				for _, member := range spec.Parallel {
					if candidate.Name == member {
						return fmt.Errorf("phase %q: rerun-chain from %q would replay parallel member %q; "+
							"a chain replay cannot include the parallel group", phase.Name, edge.Run, member)
					}
				}
			}
			if candidate.Name == phase.Name {
				break
			}
		}
	}
	if edge.Budget <= 0 {
		return fmt.Errorf(
			"phase %q: repair edge has no positive budget — unbounded repair loops are rejected",
			phase.Name)
	}
	switch edge.Exhausted {
	case "", RepairExhaustedFailJob, RepairExhaustedProceed:
	default:
		return fmt.Errorf("phase %q: repair edge \"exhausted\" must be %q or %q, got %q",
			phase.Name, RepairExhaustedFailJob, RepairExhaustedProceed, edge.Exhausted)
	}
	switch phase.Kind {
	case PhaseKindAgent:
		if edge.When == "" {
			return fmt.Errorf(
				"phase %q: agent-phase repair edge requires a \"when\" envelope predicate",
				phase.Name)
		}
		if _, err := ParsePredicate(edge.When); err != nil {
			return fmt.Errorf("phase %q: repair edge: %w", phase.Name, err)
		}
	case PhaseKindCode:
		if edge.When != "" {
			return fmt.Errorf(
				"phase %q: code-phase failure is nonzero exit — remove the \"when\" predicate",
				phase.Name)
		}
	}
	return nil
}

// validateRepairAcyclic rejects cycles in the repair-target graph. Each edge
// carries its own budget, but a cycle of edges multiplies budgets into a
// loop no single declaration bounds — so any cycle among on_fail targets is
// rejected at save time, named by its member phases (KTD2).
func (spec *DefinitionSpec) validateRepairAcyclic(phases map[string]PhaseSpec) error {
	for _, start := range spec.Phases {
		if start.OnFail == nil {
			continue
		}
		visited := []string{start.Name}
		current := start
		for current.OnFail != nil {
			next := phases[current.OnFail.Run]
			for _, seen := range visited {
				if seen == next.Name {
					return fmt.Errorf(
						"repair cycle %s -> %q: cycles beyond a declared budget are rejected",
						strings.Join(quoteAll(visited), " -> "), next.Name)
				}
			}
			visited = append(visited, next.Name)
			current = next
		}
	}
	return nil
}

func (spec *DefinitionSpec) validateAcceptance() error {
	declaredGates := make(map[string]bool)
	for _, phase := range spec.Phases {
		for _, gate := range phase.Gates {
			declaredGates[gate.Name] = true
		}
	}
	for _, check := range spec.Acceptance {
		if check == CheckAllPhasesPassed || declaredGates[check] {
			continue
		}
		return fmt.Errorf(
			"acceptance check %q does not resolve to %q or a gate declared in the chain",
			check, CheckAllPhasesPassed)
	}
	return nil
}

// RequiredEnvNames is the union of every roster role's env allowlist, sorted
// and deduplicated. Claim eligibility requires this set to be a subset of the
// worker's advertised env names, so a job missing a variable fails at claim,
// not N phases deep (R17, KTD10).
func (spec *DefinitionSpec) RequiredEnvNames() []string {
	seen := make(map[string]bool)
	var names []string
	for _, role := range spec.Roster {
		for _, name := range role.Env {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// validateGateAllow checks a gate's allow list at save time (R1): only
// tests_intact reads one, so on any other gate it is a misconfiguration that
// would otherwise sit there looking like an exemption. Each entry must be a
// well-formed path.Match glob. `**` is refused rather than accepted, because
// path.Match reads it as a plain `*` that stops at `/` — the writes
// allowlist's `**` crosses directories, and an allow entry that quietly
// meant less than it says would fail the agent it was written to exempt.
func validateGateAllow(gate GateSpec) error {
	if gate.Allow == nil {
		return nil
	}
	if gate.Name != GateTestsIntact {
		return fmt.Errorf("allow is only meaningful on %q", GateTestsIntact)
	}
	for _, pattern := range gate.Allow {
		if strings.TrimSpace(pattern) == "" {
			return fmt.Errorf("allow entries must be non-empty globs")
		}
		if strings.Contains(pattern, "**") {
			return fmt.Errorf("allow glob %q: `**` is not supported; allow uses path.Match, where `*` stops at `/`", pattern)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("allow glob %q is malformed: %w", pattern, err)
		}
	}
	return nil
}

// validateBuildOutputs checks the build_outputs grant at save time. Every
// entry must be a DIRECTORY grant, because the write boundary sees a
// wholly-ignored directory as one collapsed `dir/` entry: a file-level
// pattern (`bin/*.o`) could never match that entry for an ignored file while
// matching the same file unignored, so ignored and unignored would disagree.
// A directory grant ends in `/**` (glob characters allowed before it) or is
// a literal `dir/` with no glob character — a trailing-slash pattern is a
// literal prefix, never a wildcard. Whether a path is tracked in HEAD is
// decided at enforcement time, not here.
func (spec *DefinitionSpec) validateBuildOutputs() error {
	for _, raw := range spec.BuildOutputs {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			return fmt.Errorf("build_outputs: %q: entries must be non-empty directory patterns", raw)
		}
		if err := validateBuildOutput(pattern); err != nil {
			return fmt.Errorf("build_outputs: %q: %w", raw, err)
		}
	}
	return nil
}

func validateBuildOutput(pattern string) error {
	if strings.ContainsAny(pattern, "\x00\n\r") {
		return fmt.Errorf("must not contain NUL or a line break")
	}
	if strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("must be repository-relative, not absolute")
	}
	if strings.HasPrefix(pattern, "-") || strings.HasPrefix(pattern, ":") {
		return fmt.Errorf("must not start with %q", pattern[:1])
	}
	for _, segment := range strings.Split(pattern, "/") {
		switch segment {
		case "..":
			return fmt.Errorf("must not contain a `..` segment")
		case ".git":
			return fmt.Errorf("must not name a `.git` directory; git metadata is never a build output")
		}
	}
	if strings.Trim(pattern, "*?/") == "" {
		return fmt.Errorf("must name at least one literal character; a pattern of only globs grants everything")
	}
	switch {
	case strings.HasSuffix(pattern, "/**"):
		return nil
	case strings.HasSuffix(pattern, "/"):
		if strings.ContainsAny(pattern, "*?") {
			return fmt.Errorf("a trailing-slash pattern is a literal directory prefix; " +
				"use `<dir>/**` for a wildcard directory")
		}
		return nil
	default:
		return fmt.Errorf("must be a directory grant ending in `/**` or `/`; file-level patterns are refused")
	}
}

func builtinGateNames() []string {
	names := make([]string, 0, len(builtinGates))
	for name := range builtinGates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func quoteAll(values []string) []string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	return quoted
}
