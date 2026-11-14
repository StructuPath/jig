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

func TestRepairChainCannotReplayTheParallelGroup(t *testing.T) {
	phases := strings.Replace(panelPhases, `command: "true"}`,
		`command: "false", on_fail: {run: build, then: rerun-chain, budget: 1}}`, 1)
	mustParse(t, parallelPanel("", phases, ""))
	_, err := ParseDefinition([]byte(parallelPanel("", phases, "parallel: [review-a, review-b, review-c]\n")))
	if err == nil || !strings.Contains(err.Error(), "parallel member") {
		t.Fatalf("rerun-chain across the parallel group: got %v", err)
	}
}

// rerun-self dispatches the repair target alone just as rerun-chain does, so
// a parallel member is no valid target for either.
func TestARepairEdgeCannotTargetAParallelMember(t *testing.T) {
	for _, then := range []string{RepairThenRerunSelf, RepairThenRerunChain} {
		t.Run(then, func(t *testing.T) {
			phases := strings.Replace(panelPhases, `{name: check, kind: code, command: "true"}`,
				`{name: check, kind: code, command: "false", on_fail: {run: review-b, then: `+then+`, budget: 1}}`, 1)
			mustParse(t, parallelPanel("", phases, ""))
			mustReject(t, parallelPanel("", phases, "parallel: [review-a, review-b, review-c]\n"),
				`phase "check"`, `"review-b"`, "parallel")
		})
	}
}
