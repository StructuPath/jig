// params.go — the run parameters a definition's prompts expect, so an
// unset {{name}} is refused before admission rather than reaching an agent
// literally.
package protocol

import (
	"regexp"
	"slices"
	"sort"
)

// Built-in template variables are never run parameters an operator passes:
// {{prompt}} is composed from the invocation's instructions at admission
// (R2), and the engine supplies {{previous_envelope}} and {{handoff_dir}}
// itself when it renders each user prompt (composePrompt in
// internal/engine/phase.go).
const (
	PreviousEnvelopeVariable = "previous_envelope"
	HandoffDirVariable       = "handoff_dir"
)

func isBuiltinTemplateVariable(name string) bool {
	return name == PromptParameter || name == PreviousEnvelopeVariable || name == HandoffDirVariable
}

// templatePlaceholder is a {{name}} the engine would substitute. The engine
// has no key syntax of its own: composePrompt (internal/engine/phase.go)
// replaces the literal text "{{" + name + "}}" for every frozen parameter,
// so {{focus.area}}, {{.Go}} or {{repo/path}} substitute as readily as
// {{focus}}. The name here is therefore any run of characters an operator
// can pass with `--param name=value`, bounded by the two things that cannot
// be a passable name: "=" (parseParameters splits at the first one) and a
// brace (it would be a different placeholder's delimiter). Whitespace is
// excluded too, on purpose: {{ x }} is how GitHub Actions (${{ github.sha }}),
// Handlebars and Go templates write their own expressions, which a prompt
// quotes as text and the engine leaves literal; refusing those would leave
// an operator no way to admit the run without corrupting the prompt.
var templatePlaceholder = regexp.MustCompile(`\{\{([^\s{}=]+)\}\}`)

// ParameterReference is one {{name}} a role's inline user prompt expects a
// run parameter for.
type ParameterReference struct {
	Role string
	Name string
}

// ReferencedParameters lists the run parameters the definition's agent
// phases reference in their inline user prompts, built-in variables
// excluded, by role name and then order of first appearance. An unset
// parameter is not an error at render time — the engine leaves {{name}}
// literal — so this is how a caller refuses it before admission instead of
// handing an agent a placeholder. A user_prompt_path is read from the
// worktree at run time and cannot be inspected here; roles that only lend a
// code phase their env never render a prompt.
func (spec *DefinitionSpec) ReferencedParameters() []ParameterReference {
	owners := map[string]bool{}
	for _, phase := range spec.Phases {
		if phase.Kind == PhaseKindAgent && phase.Owner != "" {
			owners[phase.Owner] = true
		}
	}
	roles := make([]string, 0, len(owners))
	for role := range owners {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	var references []ParameterReference
	for _, role := range roles {
		seen := map[string]bool{}
		for _, match := range templatePlaceholder.FindAllStringSubmatch(spec.Roster[role].UserPrompt, -1) {
			name := match[1]
			if isBuiltinTemplateVariable(name) || seen[name] {
				continue
			}
			seen[name] = true
			references = append(references, ParameterReference{Role: role, Name: name})
		}
	}
	return references
}

// ExpectedParameters is the sorted set of parameter names
// ReferencedParameters reports: what `jig def validate` tells an author the
// definition needs passed with --param.
func (spec *DefinitionSpec) ExpectedParameters() []string {
	var names []string
	for _, reference := range spec.ReferencedParameters() {
		if !slices.Contains(names, reference.Name) {
			names = append(names, reference.Name)
		}
	}
	sort.Strings(names)
	return names
}
