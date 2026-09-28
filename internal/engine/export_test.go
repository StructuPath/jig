package engine

import "github.com/StructuPath/jig/internal/worker"

// FieldViewOf exposes a kept chain's merged field view to the external test
// package: the view guards and the publish hold read is part of what a
// parallel group must merge exactly as sequential execution would.
func FieldViewOf(continuation worker.Continuation) map[string]any {
	return continuation.(*ciContinuation).e.fieldView
}
