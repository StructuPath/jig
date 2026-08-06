// accept.go — the acceptance predicate (R12's evaluation half), worker-side
// with evidence. Acceptance is composed of named checks: the built-in
// all_phases_passed — always evaluated, because R12 makes "all phases
// passed" a hard conjunct whether or not the definition names it — plus
// declared references to gates in the chain, judged by that gate's final
// recorded report. A predicate failure is its own terminal cause, distinct
// from a phase failure: every phase can succeed while the declared bar was
// still not met.
package engine

import (
	"fmt"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// acceptanceResult is the evaluated predicate with per-check evidence.
type acceptanceResult struct {
	Passed bool                 `json:"passed"`
	Checks []protocol.GateCheck `json:"checks"`
}

// evaluateAcceptance judges the finished chain. finalStatus is the LAST
// recorded status per phase name (a phase that failed and then passed on a
// repair rerun passed); skipped phases pass by construction — an `if:`
// guard that held false is the definition behaving, not work left undone.
func evaluateAcceptance(
	spec *protocol.DefinitionSpec,
	results []protocol.PhaseResult,
	gateReports map[string]protocol.GateReport,
) acceptanceResult {
	finalStatus := make(map[string]string, len(spec.Phases))
	for _, result := range results {
		finalStatus[result.Phase] = result.Status
	}

	outcome := acceptanceResult{Passed: true}
	record := func(item string, ok bool, note string) {
		outcome.Checks = append(outcome.Checks, protocol.GateCheck{Item: item, Ok: ok, Note: note})
		if !ok {
			outcome.Passed = false
		}
	}

	var failed []string
	executed := 0
	for _, phase := range spec.Phases {
		status, ran := finalStatus[phase.Name]
		if !ran {
			failed = append(failed, phase.Name+" (never ran)")
			continue
		}
		if status == phaseStatusSkipped {
			continue
		}
		executed++
		if status != protocol.EnvelopeSuccess {
			failed = append(failed, phase.Name)
		}
	}
	if len(failed) > 0 {
		record(protocol.CheckAllPhasesPassed, false,
			"failed: "+strings.Join(failed, ", "))
	} else {
		record(protocol.CheckAllPhasesPassed, true,
			fmt.Sprintf("%d phase(s) passed", executed))
	}

	for _, check := range spec.Acceptance {
		if check == protocol.CheckAllPhasesPassed {
			continue // already evaluated above, never twice
		}
		report, recorded := gateReports[check]
		if !recorded {
			record(check, false, "gate never ran")
			continue
		}
		if report.Passed() {
			record(check, true, fmt.Sprintf("%d check(s) verified", len(report.Checks)))
			continue
		}
		record(check, false, strings.Join(report.Violations(), "; "))
	}
	return outcome
}
