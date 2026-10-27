package main

import (
	"testing"
	"time"
)

func TestCancellationMergeStopsOnCompletionOrEitherInput(t *testing.T) {
	for _, cause := range []string{"completion", "signal", "job"} {
		t.Run(cause, func(t *testing.T) {
			signal, job := make(chan struct{}), make(chan struct{})
			cancelled, stop := anyClosed(signal, job)
			defer stop()
			switch cause {
			case "completion":
				stop()
			case "signal":
				close(signal)
			case "job":
				close(job)
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("cancellation waiters did not stop")
			}
			// stop waits for all helpers, including the input that never closed.
			stop()
		})
	}
}
