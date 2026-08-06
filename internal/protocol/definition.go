package protocol

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

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
}

// Repair-edge continuation and exhaustion policies (KTD2).
const (
	RepairThenRerunSelf = "rerun-self"

	RepairExhaustedFailJob = "fail-job"
	RepairExhaustedProceed = "proceed"
)

// DefinitionSpec is the parsed YAML form of a Job Definition (R1, KTD2): an
// ordered phase chain, a per-role roster, and an acceptance predicate of
// named checks. Validate enforces the save-time contract; nothing downstream
// re-checks it.
type DefinitionSpec struct {
	Name       string              `yaml:"name"`
	Roster     map[string]RoleSpec `yaml:"roster"`
	Phases     []PhaseSpec         `yaml:"phases"`
	Acceptance []string            `yaml:"acceptance"`
}

// RoleSpec is one roster entry: the model, prompts, and allowlists an agent
// role runs with. Prompts are content or a repo-relative path, exclusively.
// Env is the per-role environment allowlist (KTD10); SensitiveEnv names the
// subset whose values are redacted from every persisted trace (R15).
type RoleSpec struct {
	Model            string   `yaml:"model"`
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
// role's environment). If names an envelope field: the phase is skipped
// unless a previous envelope set that field truthy (KTD2).
type PhaseSpec struct {
	Name    string      `yaml:"name"`
	Kind    string      `yaml:"kind"`
	Owner   string      `yaml:"owner"`
	Command string      `yaml:"command"`
	If      string      `yaml:"if"`
	Gates   []GateSpec  `yaml:"gates"`
	OnFail  *RepairEdge `yaml:"on_fail"`
}

// GateSpec configures one built-in gate on a phase. Budget is the
// gate-correction retry budget; zero means DefaultPhaseRetryBudget. Command
// is required by tests_pass and meaningless to every other gate.
type GateSpec struct {
	Name    string `yaml:"name"`
	Command string `yaml:"command"`
	Budget  int    `yaml:"budget"`
}

// RepairEdge is the one declared loop construct (KTD2):
//
//	on_fail: {run: <phase>, then: rerun-self, budget: N, exhausted: fail-job|proceed}
//
// Failure that triggers the edge is nonzero exit for code phases and the
// declared When envelope predicate for agent phases. Budget bounds the loop;
// an edge without a positive budget is rejected at save time.
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
	return spec.validateAcceptance()
}

func (role RoleSpec) validate(name string) error {
	if strings.TrimSpace(role.Model) == "" {
		return fmt.Errorf("role %q: model is required", name)
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
	case PhaseKindCode:
		if strings.TrimSpace(phase.Command) == "" {
			return fmt.Errorf("phase %q: code phases require a command", phase.Name)
		}
	default:
		return fmt.Errorf("phase %q: kind %q is not %q or %q",
			phase.Name, phase.Kind, PhaseKindAgent, PhaseKindCode)
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
	if edge.Then != RepairThenRerunSelf {
		return fmt.Errorf("phase %q: repair edge \"then\" must be %q, got %q",
			phase.Name, RepairThenRerunSelf, edge.Then)
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
