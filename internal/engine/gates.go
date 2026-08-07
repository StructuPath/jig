// gates.go — the built-in claim-verifier registry (R9). A gate verifies the
// envelope's CLAIMS against the worktree, never guesses about quality: an
// artifact the agent named must exist, a file it claims changed must be
// there, a verdict must agree with its own findings, a test command must
// exit 0. Every check is recorded either way — a green gate says WHAT it
// verified, and on a failed check the note doubles as the correction the
// agent is told. Repo-specific verification beyond this registry is a code
// phase, not a new gate.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/StructuPath/jig/internal/protocol"
)

// gateContext is what a gate check may look at: the envelope under
// question and the worktree its claims are about.
type gateContext struct {
	ctx      context.Context
	worktree string
	envelope parsedEnvelope
	command  string // tests_pass only
	// env is the complete subprocess environment a gate command runs with:
	// the owning role's allowlist plus the attempt's ephemeral HOME (KTD10,
	// KTD11). A gate command runs in the worktree the agent just wrote — a
	// `go test` or `npm test` gate executes agent-authored code — so it is
	// contained exactly like the agent that wrote it, never with jig's own
	// environment and the operator's real HOME.
	env     []string
	timeout timeoutConfig
}

// runGate dispatches one configured gate by registry name. Names were
// resolved at save time (R1); an unknown name here is a frozen-snapshot
// integrity failure, reported as a failed check rather than a panic.
func runGate(gc gateContext, spec protocol.GateSpec) protocol.GateReport {
	gc.command = spec.Command
	switch spec.Name {
	case protocol.GateArtifactsExist:
		return gateArtifactsExist(gc)
	case protocol.GateFilesNonEmpty:
		return gateFilesNonEmpty(gc)
	case protocol.GateDiffMatchesClaims:
		return gateDiffMatchesClaims(gc)
	case protocol.GateVerdictConsistent:
		return gateVerdictConsistent(gc)
	case protocol.GateTestsPass:
		return gateTestsPass(gc)
	}
	var report protocol.GateReport
	report.Check(spec.Name, false, "gate is not in the built-in registry")
	return report
}

// artifactPath resolves a claimed artifact against the worktree, rejecting
// absolute paths and traversal — an artifact claim may only name worktree
// content.
func artifactPath(worktree, claimed string) (string, bool) {
	if claimed == "" || filepath.IsAbs(claimed) || !filepath.IsLocal(claimed) {
		return "", false
	}
	return filepath.Join(worktree, claimed), true
}

func fileSize(info os.FileInfo) string {
	n := info.Size()
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1fKB", float64(n)/1024)
}

// gateArtifactsExist: every declared artifact exists in the worktree.
func gateArtifactsExist(gc gateContext) protocol.GateReport {
	var report protocol.GateReport
	for _, artifact := range gc.envelope.Base.Artifacts {
		path, ok := artifactPath(gc.worktree, artifact)
		if !ok {
			report.Check(artifact, false, "declared artifact path is not inside the worktree")
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			report.Check(artifact, false, "declared artifact does not exist")
			continue
		}
		report.Check(artifact, true, "exists, "+fileSize(info))
	}
	return report
}

// gateFilesNonEmpty: existing declared artifacts are not empty. Existence
// itself is artifacts_exist's job.
func gateFilesNonEmpty(gc gateContext) protocol.GateReport {
	var report protocol.GateReport
	for _, artifact := range gc.envelope.Base.Artifacts {
		path, ok := artifactPath(gc.worktree, artifact)
		if !ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Size() == 0 {
			report.Check(artifact, false, "declared artifact is empty")
			continue
		}
		report.Check(artifact, true, fileSize(info))
	}
	return report
}

// gateDiffMatchesClaims: every file the envelope claims changed exists on
// disk — an agent reporting work it never wrote is refuted mechanically.
func gateDiffMatchesClaims(gc gateContext) protocol.GateReport {
	var report protocol.GateReport
	// A gate that records nothing PASSES (Passed() is "no violations"), so an
	// absent or non-array changed_files would make this gate a no-op for
	// exactly the agent that declined to answer it. The claim is the thing
	// being checked: no claim is a failed check, not an exemption.
	raw, present := gc.envelope.Fields["changed_files"]
	claimed, isList := raw.([]any)
	switch {
	case !present:
		report.Check("changed_files", false, "the envelope claims no changed_files")
		return report
	case !isList:
		report.Check("changed_files", false,
			fmt.Sprintf("changed_files must be a list of paths, got %T", raw))
		return report
	case len(claimed) == 0:
		// An empty list is an answer: the phase claims it changed nothing.
		// Recorded so the green report says what it checked (R9).
		report.Check("changed_files", true, "the phase claims no changed files")
		return report
	}
	for _, item := range claimed {
		name, ok := item.(string)
		if !ok {
			report.Check(fmt.Sprintf("%v", item), false, "claimed changed file is not a path string")
			continue
		}
		path, ok := artifactPath(gc.worktree, name)
		if !ok {
			report.Check(name, false, "claimed changed file is not inside the worktree")
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			report.Check(name, false, "claimed changed file does not exist")
			continue
		}
		report.Check(name, true, "exists, "+fileSize(info))
	}
	return report
}

// gateVerdictConsistent: a review's verdict must agree with the findings it
// just wrote down. Nothing here judges the code — this checks the envelope
// against ITSELF: an approval shipping blocking items, or a rejection that
// names no problem, is a claim the harness refutes without reading a line.
func gateVerdictConsistent(gc gateContext) protocol.GateReport {
	var report protocol.GateReport
	// Presence and type are kept apart from the value. An absent field and a
	// stated `false` both read as false, and telling an agent it claimed
	// approved=false when it claimed nothing sends it looking for a decision
	// it never made — whose cheapest repair is to add approved=true, turning
	// a missing verdict into an approval.
	rawApproved, approvedPresent := gc.envelope.Fields["approved"]
	approved, approvedIsBool := rawApproved.(bool)
	blocking, _ := gc.envelope.Fields["blocking"].([]any)
	var unmet int
	if findings, ok := gc.envelope.Fields["findings"].([]any); ok {
		for _, item := range findings {
			finding, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if met, ok := finding["met"].(bool); ok && !met {
				unmet++
			}
		}
	}
	switch {
	case len(blocking) == 0:
		report.Check("approved vs blocking", true, "no blocking items")
	case approved:
		report.Check("approved vs blocking", false,
			fmt.Sprintf("%d blocking item(s) while approved=true", len(blocking)))
	default:
		report.Check("approved vs blocking", true,
			fmt.Sprintf("%d blocking item(s), not approved", len(blocking)))
	}
	switch {
	case unmet == 0:
		report.Check("approved vs findings", true, "every requirement met")
	case approved:
		report.Check("approved vs findings", false,
			fmt.Sprintf("%d unmet requirement(s) while approved=true", unmet))
	default:
		report.Check("approved vs findings", true,
			fmt.Sprintf("%d unmet requirement(s), not approved", unmet))
	}
	switch {
	case approved || len(blocking) > 0 || unmet > 0:
		report.Check("rejection names a problem", true, "verdict is supported")
	case !approvedPresent:
		report.Check("rejection names a problem", false,
			`"approved" is missing: state the verdict explicitly as true or false. `+
				"It is not inferred, and a missing verdict is not a rejection to justify.")
	case !approvedIsBool:
		report.Check("rejection names a problem", false,
			fmt.Sprintf(`"approved" must be a JSON boolean, got %T: `+
				"state the verdict explicitly as true or false", rawApproved))
	default:
		report.Check("rejection names a problem", false,
			"approved=false but no blocking item or unmet requirement was given")
	}
	return report
}

// gateTestsPass: the configured command must exit 0 in the worktree. The
// note carries the exit status and, on failure, a bounded output tail as
// evidence the agent can act on.
func gateTestsPass(gc gateContext) protocol.GateReport {
	var report protocol.GateReport
	result := runShellCommand(gc.ctx, gc.worktree, gc.command, gc.env, gc.timeout.phase)
	note := fmt.Sprintf("exit %d", result.ExitCode)
	if result.TimedOut {
		note = "timed out"
	}
	if !result.Passed() {
		note += "\n" + result.OutputTail
	}
	report.Check(gc.command, result.Passed(), note)
	return report
}
