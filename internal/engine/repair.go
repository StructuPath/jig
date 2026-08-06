// repair.go — the declared repair-edge construct (KTD2) and the predicate
// language it keys on. An edge is data — `on_fail: {run, then, budget,
// exhausted}` — evaluated by the chain executor: nonzero exit fails a code
// phase, the declared envelope predicate "fails" an agent phase (an
// envelope can be valid, gated, and status=success while still rejecting —
// `approved == false` is a verdict, not a malfunction). This file owns the
// pure evaluation halves; the loop itself lives in phase.go.
package engine

import (
	"fmt"
	"strconv"

	"github.com/StructuPath/jig/internal/protocol"
)

// predicateHolds evaluates one declared `<field> ==|!= <literal>` predicate
// against an envelope's field map. Literals compare by the field's own
// type: booleans as true/false, numbers numerically, null as absence-or-
// null, everything else as strings. A missing field only ever matches the
// literal `null`.
func predicateHolds(predicate protocol.EnvelopePredicate, fields map[string]any) bool {
	equal := literalEquals(fields[predicate.Field], predicate.Literal)
	if predicate.Op == "!=" {
		return !equal
	}
	return equal
}

func literalEquals(value any, literal string) bool {
	switch typed := value.(type) {
	case nil:
		return literal == "null"
	case bool:
		return (literal == "true") == typed
	case float64:
		number, err := strconv.ParseFloat(literal, 64)
		return err == nil && number == typed
	case string:
		return typed == literal
	default:
		return fmt.Sprintf("%v", typed) == literal
	}
}

// truthy is the `if: <envelope-field>` guard test (KTD2): the phase runs
// iff a previous envelope set the field to something other than false,
// zero, empty, or null.
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0
	case string:
		return typed != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// edgeExhaustionFailsJob reads the edge's declared exhaustion policy:
// fail-job is the default, proceed is the declared exception (KTD2).
func edgeExhaustionFailsJob(edge *protocol.RepairEdge) bool {
	return edge.Exhausted != protocol.RepairExhaustedProceed
}
