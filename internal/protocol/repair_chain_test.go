package protocol

import (
	"strings"
	"testing"
)

func TestRepairChainRequiresAnEarlierTargetAndPositiveBudget(t *testing.T) {
	source := `name: repair-chain
phases:
  - {name: plan, kind: code, command: "true"}
  - {name: build, kind: code, command: "true"}
  - name: test
    kind: code
    command: "false"
    on_fail: {run: plan, then: rerun-chain, budget: 1, exhausted: fail-job}
  - {name: later, kind: code, command: "true"}
acceptance: [all_phases_passed]
`
	if _, err := ParseDefinition([]byte(source)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{strings.Replace(source, "run: plan", "run: later", 1), strings.Replace(source, "budget: 1", "budget: 0", 1)} {
		if _, err := ParseDefinition([]byte(invalid)); err == nil {
			t.Fatal("invalid repair chain accepted")
		}
	}
}
