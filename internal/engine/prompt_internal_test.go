package engine

import (
	"strings"
	"testing"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// The missing_param check (protocol.ReferencedParameters) must report
// exactly the placeholders composePrompt would substitute: one it misses
// reaches the agent literally, one it invents refuses a run for nothing.
func TestReferencedParametersAreExactlyWhatComposePromptSubstitutes(t *testing.T) {
	template := "{{prompt}} {{focus}} {{focus.area}} {{.Go}} {{repo/path}} {{max-depth}} {{{braced}}} " +
		"{{previous_envelope}} {{handoff_dir}} literal {{ spaced }} ${{ github.sha }}"
	spec, err := protocol.ParseDefinition([]byte(`
name: mirror
roster:
  agent:
    model: haiku
    system_prompt: s
    user_prompt: "` + template + `"
    writes: []
phases:
  - {name: one, kind: agent, owner: agent}
`))
	if err != nil {
		t.Fatal(err)
	}
	parameters := map[string]string{protocol.PromptParameter: "P"}
	for _, reference := range spec.ReferencedParameters() {
		parameters[reference.Name] = "<" + reference.Name + ">"
	}
	text := composePrompt(template, parameters, nil, "/handoff")
	for name, value := range parameters {
		if !strings.Contains(text, value) || strings.Contains(text, "{{"+name+"}}") {
			t.Errorf("reported parameter %q was not substituted:\n%s", name, text)
		}
	}
	// What remains is only what the check deliberately leaves literal.
	body := text[:strings.Index(text, "\n\n")]
	want := "P <focus> <focus.area> <.Go> <repo/path> <max-depth> {<braced>} (none) /handoff " +
		"literal {{ spaced }} ${{ github.sha }}"
	if body != want {
		t.Fatalf("composed prompt = %q, want %q", body, want)
	}
}
