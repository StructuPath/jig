package protocol

import (
	"slices"
	"testing"
)

func TestReferencedParametersExcludeBuiltinsAndNonRenderingRoles(t *testing.T) {
	spec, err := ParseDefinition([]byte(`
name: survey
roster:
  surveyor:
    model: haiku
    system_prompt: You survey.
    user_prompt: |
      Task: {{prompt}} focus on {{focus}} then {{focus}} again, scope {{scope}}.
      Previous: {{previous_envelope}} notes in {{handoff_dir}}; literal {{ spaced }} and ${{ github.sha }}.
      Keyed: {{focus.area}} {{.Go}} {{repo/path}} {{max-depth}} {{{braced}}} {{a=b}} {{}}.
    writes: []
  linter:
    model: haiku
    system_prompt: unused as an agent
    user_prompt: "{{never_rendered}}"
phases:
  - {name: survey, kind: agent, owner: surveyor}
  - {name: lint, kind: code, owner: linter, command: "true"}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []ParameterReference{
		{Role: "surveyor", Name: "focus"}, {Role: "surveyor", Name: "scope"},
		{Role: "surveyor", Name: "focus.area"}, {Role: "surveyor", Name: ".Go"},
		{Role: "surveyor", Name: "repo/path"}, {Role: "surveyor", Name: "max-depth"},
		{Role: "surveyor", Name: "braced"},
	}
	if got := spec.ReferencedParameters(); !slices.Equal(got, want) {
		t.Fatalf("ReferencedParameters = %v, want %v", got, want)
	}
	if got := spec.ExpectedParameters(); !slices.Equal(got, []string{".Go", "braced", "focus", "focus.area", "max-depth", "repo/path", "scope"}) {
		t.Fatalf("ExpectedParameters = %v", got)
	}
}
