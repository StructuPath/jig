package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// killBackgrounded stops the process a test command left running, read from
// the pid file the command wrote.
func killBackgrounded(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// `server & go test` exits as soon as the test does, but the backgrounded
// server still holds the output pipe. The gate must be judged on the shell's
// exit, not left waiting for the server until the phase timeout.
func TestACommandThatBackgroundsAProcessReturnsOnTheShellsExit(t *testing.T) {
	cases := []struct {
		name     string
		tail     string
		wantExit int
	}{
		{name: "clean exit", tail: "echo done", wantExit: 0},
		{name: "failing exit", tail: "echo done; exit 3", wantExit: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "sleep.pid")
			killBackgrounded(t, pidFile)
			command := "sleep 600 & echo $! > " + pidFile + "; " + tc.tail

			started := time.Now()
			result := runShellCommand(t.Context(), t.TempDir(), command,
				[]string{"PATH=" + os.Getenv("PATH")}, time.Minute, nil)
			elapsed := time.Since(started)

			if elapsed > 3*commandWaitDelay {
				t.Fatalf("command took %s; Wait blocked on the backgrounded process", elapsed)
			}
			if result.TimedOut || result.StartError != "" || result.ExitCode != tc.wantExit {
				t.Fatalf("result = %+v, want exit %d judged as a normal exit", result, tc.wantExit)
			}
			if !strings.Contains(result.OutputTail, "done") {
				t.Fatalf("output tail %q lost the command's output", result.OutputTail)
			}
		})
	}
}

// A cancelled phase kills the command's group and returns, even when a
// process outside the group still holds the output pipe.
func TestACancelledCommandReturnsWhileAnEscapedProcessHoldsThePipe(t *testing.T) {
	if _, err := os.Stat("/usr/bin/perl"); err != nil {
		t.Skip("needs perl to start a process in a new session")
	}
	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	killBackgrounded(t, pidFile)
	// setsid moves the sleeper out of the command's process group, so the
	// group kill on timeout cannot reach it.
	command := `/usr/bin/perl -e 'use POSIX; if (fork) { exit 0 } POSIX::setsid(); ` +
		`open(F, ">", $ARGV[0]); print F $$; close F; exec "sleep", "600"' ` + pidFile + `; sleep 600`

	started := time.Now()
	result := runShellCommand(t.Context(), t.TempDir(), command,
		[]string{"PATH=" + os.Getenv("PATH")}, time.Second, nil)
	elapsed := time.Since(started)

	if !result.TimedOut {
		t.Fatalf("result = %+v, want a timeout", result)
	}
	if elapsed > time.Second+3*commandWaitDelay {
		t.Fatalf("cancelled command took %s to return", elapsed)
	}
}
