// live_test.go — the U10 live smoke against the REAL Codex CLI. Guarded so
// CI never pays for it: JIG_LIVE_SMOKE_CODEX=1 to run.
//
// It is the adapter-level shape of U10's acceptance criterion: a two-send
// chain where the second send is a forced parse-correction that must land in
// the SAME live thread, plus the probed version and capability flags that
// registration records. The end-to-end `jig run` with a Codex-rostered
// definition additionally needs the definition/CLI wiring outside this
// package; this proves the runtime half of it.
package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/runtime"
)

func TestLiveSmokeRealCodex(t *testing.T) {
	if os.Getenv("JIG_LIVE_SMOKE_CODEX") != "1" {
		t.Skip("set JIG_LIVE_SMOKE_CODEX=1 to run the real-CLI smoke")
	}
	adapter, err := New()
	if err != nil {
		t.Skipf("codex CLI unavailable: %v", err)
	}
	capability, err := adapter.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("probed %+v", capability)
	if capability.Version == "" {
		t.Fatal("registration would carry no CLI version")
	}
	if capability.ReportsCost {
		t.Fatal("codex reports no dollar cost; reports_cost must stay false")
	}

	workDir := t.TempDir()
	session := &runtime.Session{Key: "live-smoke/writer"}
	environment := os.Environ()

	// Phase one: do the work and answer in the envelope shape.
	first := live(t, adapter, session, `Create a file named hello.txt in the current directory `+
		`containing exactly the text: hello jig
Then reply with ONLY this JSON and nothing else: {"status": "done", "file": "hello.txt"}`,
		workDir, environment)
	if session.NativeID == "" {
		t.Fatal("the session never learned its codex thread id")
	}
	t.Logf("send 1: thread=%s usage=%+v text=%q", first.SessionID, first.Usage, first.Text)
	content, readErr := os.ReadFile(filepath.Join(workDir, "hello.txt"))
	if readErr != nil {
		t.Fatalf("the agent did not write hello.txt: %v", readErr)
	}
	if !strings.Contains(string(content), "hello jig") {
		t.Fatalf("hello.txt = %q", content)
	}
	if first.Usage.TotalTokens == 0 {
		t.Error("no token accounting reported for the first send")
	}
	if first.Usage.CostUSD != 0 {
		t.Errorf("usage = %+v, want no invented dollar cost", first.Usage)
	}

	if !capability.CanResume {
		t.Skip("this build reports can_resume=false; the correction half is the engine's " +
			"transcript-digest path, not the adapter's")
	}

	// Phase two: the forced parse-correction, into the SAME live thread. If
	// the CLI cold started instead, Result returns ErrSessionDiscontinuity and
	// this fails rather than silently losing the conversation.
	askedToResume := session.NativeID
	second := live(t, adapter, session, `That reply could not be parsed as an envelope. `+
		`Re-emit your previous answer as strict JSON on a single line with the keys `+
		`"status" and "file", and no other text.`, workDir, environment)
	t.Logf("send 2: thread=%s usage=%+v text=%q", second.SessionID, second.Usage, second.Text)
	if second.SessionID != askedToResume {
		t.Fatalf("resume answered under %s, asked for %s", second.SessionID, askedToResume)
	}
	if !strings.Contains(second.Text, "hello.txt") {
		t.Errorf("the correction lost the conversation: %q", second.Text)
	}
	if second.Usage.TotalTokens == 0 {
		t.Error("no token accounting reported for the correction send")
	}
}

// live runs one send end to end the way the engine does — drain the events,
// then take the result — and fails the test on any error.
func live(
	t *testing.T, adapter *Adapter, session *runtime.Session,
	prompt, workDir string, environment []string,
) runtime.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	handle, err := adapter.StartOrContinue(ctx, session, prompt, runtime.Options{
		WorkDir: workDir,
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("start send %d: %v", session.Sends, err)
	}
	events := 0
	for event := range handle.Events() {
		events++
		if event.Kind == runtime.EventToolCall {
			t.Logf("  tool_call %s", event.Name)
		}
	}
	result, err := handle.Result()
	if err != nil {
		t.Fatalf("send %d: %v (text=%q)", session.Sends, err, result.Text)
	}
	if result.IsError {
		t.Fatalf("send %d reported a failed turn: %q", session.Sends, result.Text)
	}
	if events == 0 {
		t.Errorf("send %d produced no stream events", session.Sends)
	}
	return result
}
