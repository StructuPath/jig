package engine

import (
	"testing"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// A boolean field matches only the literals true and false. Any other
// literal is no-match on both a stated true and a stated false, so `null`
// cannot hold for `approved: false` and a `True` typo cannot invert the
// comparison (KTD2).
func TestABooleanFieldMatchesOnlyTrueOrFalse(t *testing.T) {
	cases := []struct {
		value   bool
		literal string
		want    bool
	}{
		{true, "true", true},
		{true, "false", false},
		{false, "false", true},
		{false, "true", false},
		{false, "null", false},
		{true, "null", false},
		{false, "True", false},
		{true, "True", false},
		{false, "0", false},
		{false, "", false},
	}
	for _, c := range cases {
		fields := map[string]any{"approved": c.value}
		equal := protocol.EnvelopePredicate{Field: "approved", Op: "==", Literal: c.literal}
		if got := predicateHolds(equal, fields); got != c.want {
			t.Errorf("approved=%v == %q held %v, want %v", c.value, c.literal, got, c.want)
		}
	}
	// A missing field still matches only null.
	absent := protocol.EnvelopePredicate{Field: "approved", Op: "==", Literal: "null"}
	if !predicateHolds(absent, map[string]any{}) {
		t.Error("an absent field must match null")
	}
}
