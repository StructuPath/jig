package engine

import "testing"

// A group that ends on several members' terminal results reports ONE cause:
// cancellation over the ceiling over the send budget, and between equal
// causes the member declared first. (A breach outranks all three; the
// runner decides it before consulting this.)
func TestGroupTerminalCausePrecedence(t *testing.T) {
	terminal := func(index int, outcome phaseOutcome, failure string) memberTerminal {
		return memberTerminal{index: index, run: phaseRun{outcome: outcome, failure: failure}}
	}
	cases := []struct {
		name      string
		terminals []memberTerminal
		want      endKind
		diagnosis string
	}{
		{"cancellation over the ceiling", []memberTerminal{
			terminal(0, phaseCeiling, "c"), terminal(2, phaseCancelled, "x")}, endCancelled, "x"},
		{"cancellation over the send budget", []memberTerminal{
			terminal(0, phaseSendBudget, "b"), terminal(1, phaseCancelled, "x")}, endCancelled, "x"},
		{"the ceiling over the send budget", []memberTerminal{
			terminal(0, phaseSendBudget, "b"), terminal(1, phaseCeiling, "c")}, endCeiling, "c"},
		{"equal causes go to the first declared member", []memberTerminal{
			terminal(2, phaseSendBudget, "late"), terminal(0, phaseSendBudget, "first")}, endSendBudget, "first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			end := groupTerminalEnd(tc.terminals)
			if end.kind != tc.want || end.diagnostic != tc.diagnosis {
				t.Fatalf("end = %+v, want kind %d with %q", *end, tc.want, tc.diagnosis)
			}
		})
	}
}

// The send budget is one atomic check-and-take: however many members race
// for the last sends, exactly the budget is granted.
func TestConcurrentSendAcquisitionNeverOvershoots(t *testing.T) {
	counters := &attemptCounters{phaseEntries: map[string]int{}}
	const limit, racers = 50, 16
	granted := make(chan int, racers)
	for r := 0; r < racers; r++ {
		go func() {
			count := 0
			for counters.acquireSend(limit) {
				count++
			}
			granted <- count
		}()
	}
	total := 0
	for r := 0; r < racers; r++ {
		total += <-granted
	}
	if total != limit || counters.sends.Load() != limit {
		t.Fatalf("granted %d sends (counter %d), want exactly %d", total, counters.sends.Load(), limit)
	}
}

// The tightest race: many members released at once for the one send left.
// Repeated, because a check-then-add race loses only occasionally.
func TestExactlyOneMemberWinsTheLastSend(t *testing.T) {
	for round := 0; round < 500; round++ {
		counters := &attemptCounters{phaseEntries: map[string]int{}}
		counters.sends.Store(9)
		start := make(chan struct{})
		wins := make(chan bool, 32)
		for r := 0; r < 32; r++ {
			go func() {
				<-start
				wins <- counters.acquireSend(10)
			}()
		}
		close(start)
		won := 0
		for r := 0; r < 32; r++ {
			if <-wins {
				won++
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d members took the last send, want exactly 1", round, won)
		}
	}
}
