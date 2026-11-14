// unknown_field_test.go — a strict-decode rejection names the element the
// author wrote and the field they meant, never a Go type.
package protocol

import (
	"strings"
	"testing"
)

func TestAnUnknownFieldIsNamedByItsElementWithASuggestion(t *testing.T) {
	for _, tc := range []struct {
		name, source, want string
	}{
		{"role", `
name: bad
roster:
  builder:
    model: claude-sonnet
    system_prompt: s
    user_prompt: u
    writs: ["**"]
phases:
  - name: build
    kind: agent
    owner: builder
`, `line 8: role "builder": unknown field "writs" (did you mean "writes"?)`},
		{"phase", `
name: bad
phases:
  - name: test
    kind: code
    command: just check
    on_fial:
      run: build
`, `line 7: phase "test": unknown field "on_fial" (did you mean "on_fail"?)`},
		{"gate", `
name: bad
phases:
  - name: test
    kind: code
    command: just check
    gates:
      - name: tidy
        comand: go mod tidy
`, `line 9: phase "test" gate "tidy": unknown field "comand" (did you mean "command"?)`},
		{"nested", `
name: bad
phases:
  - name: test
    kind: code
    command: just check
publish:
  ci:
    wiat: true
`, `line 9: publish.ci: unknown field "wiat" (did you mean "wait"?)`},
		{"top level, no near spelling", `
name: bad
zzzzzzzz: 1
phases:
  - name: test
    kind: code
    command: just check
`, `line 3: definition: unknown field "zzzzzzzz"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDefinition([]byte(tc.source))
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %q does not contain %q", err.Error(), tc.want)
			}
			if strings.Contains(err.Error(), "protocol.") || strings.Contains(err.Error(), "did you mean") != strings.Contains(tc.want, "did you mean") {
				t.Fatalf("message %q leaks a Go type or a wrong suggestion", err.Error())
			}
		})
	}
}

func TestClosestMatchRefusesAFarSpelling(t *testing.T) {
	commands := []string{"start", "serve", "worker", "run", "def"}
	for word, want := range map[string]string{
		"serv": "serve", "wroker": "worker", "dfe": "def", "deploy": "", "x": "",
	} {
		if got := ClosestMatch(word, commands); got != want {
			t.Errorf("ClosestMatch(%q) = %q, want %q", word, got, want)
		}
	}
}
