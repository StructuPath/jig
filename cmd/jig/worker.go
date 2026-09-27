// worker.go — `jig worker` (U9): the worker host process. It probes the
// selected agent CLI, registers with the control plane, reconciles disk
// against the ledger (including the publish half's stray-branch report), then
// keeps registering and claiming until the operator stops it.
//
// Three wiring decisions carry the units' handoff notes:
//
//   - The engine is wrapped in worker.NewPublishingRunner and Bound to the
//     worker (U7). Without the wrapper an accepted chain stops at
//     `accepted_unpublished` forever: the engine deliberately ends at the
//     acceptance predicate and marks its result "publish": "not_attempted",
//     and this runner is the only thing that turns that into `accepted` with
//     remote proof.
//   - Startup reconciliation is ReconcileIncludingPublish, not Reconcile (U7).
//     The extra half reads the remote for attempt-scoped branches no fenced
//     push record explains — the residue of a zombie that pushed after its
//     lease expired. It is the only place that residue is ever surfaced, so a
//     worker that calls the plain Reconcile hides it by omission.
//   - The claim and registration loops live here rather than in
//     worker.Run, because the interrupt has to reach the ENGINE and not the
//     control-plane client: a SIGINT must stop the agent's process group and
//     still record the attempt's terminal state and reap its scratch. So the
//     signal closes a cancellation channel the engine watches, while every
//     HTTP write runs on a context detached from it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/StructuPath/jig/internal/engine"
	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/worker"
)

const workerUsage = `Usage: jig worker [--server <url>] [--data <dir>] [--runtime <name>] [--capacity <n>]

Run the worker host: register with the control plane, claim queued work,
execute each attempt's phase chain in an isolated worktree, publish accepted
work, and reconcile retained worktrees at start.

Flags:
  --server <url>         control plane base URL (default http://127.0.0.1:8383)
  --data <dir>           worker data directory: repo cache, worktrees,
                         manifests, traces, scratch (default ~/.jig/worker)
  --runtime <name>       agent CLI to run rosters on: claude-code (default)
                         or codex
  --capacity <n>         concurrent attempt slots (default 1)
  --name <name>          worker name reported at registration (default local)
  --no-seed-auth         do not seed runtime auth material into the
                         ephemeral HOME
  --commit-author-name   author name on jig's publish commit
                         (default "jig")
  --commit-author-email  author email on jig's publish commit
                         (default "jig@localhost")
  --base-branch          pull-request base branch; empty resolves the
                         repository's default branch

Exit codes:
  0  the worker stopped cleanly on SIGINT/SIGTERM
  1  infrastructure failure — the worker could not start or keep running
  2  usage error
`

// workerDrainGrace bounds how long an interrupted worker waits for in-flight
// attempts to unwind. The engine's cancellation path has to stop a process
// group, enforce the write boundary, record a terminal state, and dispose the
// worktree; cutting that short is what leaves an attempt leased with nobody
// running it.
const workerDrainGrace = 2 * time.Minute

func workerCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jig worker", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, workerUsage) }
	serverURL := flags.String("server", "http://"+defaultServerAuthority, "control plane URL")
	defaultWorkerData := ""
	if base := defaultDataDir(); base != "" {
		defaultWorkerData = filepath.Join(base, "worker")
	}
	dataDir := flags.String("data", defaultWorkerData, "worker data directory")
	runtimeName := flags.String("runtime", runtimeClaudeCode, "agent CLI runtime")
	capacity := flags.Int("capacity", 1, "concurrent attempt slots")
	name := flags.String("name", "local", "worker name")
	noSeedAuth := flags.Bool("no-seed-auth", false, "do not seed runtime auth material")
	authorName := flags.String("commit-author-name", "jig", "publish commit author name")
	authorEmail := flags.String("commit-author-email", "jig@localhost", "publish commit author email")
	baseBranch := flags.String("base-branch", "", "pull-request base branch")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != 0 || *dataDir == "" {
		flags.Usage()
		return exitUsage
	}
	if ctx == nil {
		ctx = context.Background()
	}
	logger := commandLogger(stderr)

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "jig worker: %v\n", err)
		return exitInfraFailed
	}
	selected, err := selectRuntime(ctx, *runtimeName, !*noSeedAuth)
	if err != nil {
		fmt.Fprintf(stderr, "jig worker: %v\n", err)
		return exitInfraFailed
	}
	if selected.Warning != "" {
		fmt.Fprintf(stderr, "jig worker: warning: %s\n", selected.Warning)
	}

	// Every control-plane write rides a context the interrupt does not
	// cancel: an interrupted attempt still has to report its terminal state,
	// and a cancelled HTTP client turns an orderly stop into a leased attempt
	// waiting for the sweeper (R5).
	hostCtx := context.WithoutCancel(ctx)

	// The worker, its runner, and the publish wrapper are mutually
	// referential by construction: the runner records process groups into the
	// worker's manifests, the worker is built from a config that already
	// names its runner, and publish binds to the worker afterwards. The
	// accessor closes that loop without a half-built worker ever escaping.
	var host *worker.Worker
	engineRunner := workerAttemptRunner(ctx, *dataDir, selected, logger,
		func() *worker.Worker { return host })
	publisher := worker.NewPublishingRunner(engineRunner, worker.NewGitHubCLIGateway(),
		worker.PublishOptions{
			CommitAuthorName:  *authorName,
			CommitAuthorEmail: *authorEmail,
			BaseBranch:        *baseBranch,
		})
	host, err = worker.New(worker.Config{
		ServerURL:     *serverURL,
		DataDir:       *dataDir,
		Name:          *name,
		Capacity:      *capacity,
		WorkerVersion: version,
		// The selected adapter is its own registration probe (KTD4): it is
		// richer than a version-string CommandProbe, and it is the runtime
		// this worker will actually run, so it is the one to advertise.
		Probes: []worker.RuntimeProbe{selected.Runtime},
		Runner: publisher,
		Logger: logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "jig worker: %v\n", err)
		return exitInfraFailed
	}
	// Bind before the first attempt can run: publish needs the worker for the
	// lease-fenced ledger calls and the repository cache.
	publisher.Bind(host)

	runtimes := host.ProbeRuntimes(hostCtx)
	if len(runtimes) == 0 {
		fmt.Fprintf(stderr,
			"jig worker: the %s CLI could not be probed; jobs would claim and then fail\n",
			*runtimeName)
		return exitInfraFailed
	}
	if _, err := host.RegisterOnce(hostCtx); err != nil {
		fmt.Fprintf(stderr, "jig worker: initial registration: %v\n", err)
		return exitInfraFailed
	}
	report, err := host.ReconcileIncludingPublish(hostCtx)
	if err != nil {
		fmt.Fprintf(stderr, "jig worker: startup reconciliation: %v\n", err)
		return exitInfraFailed
	}
	printReconcileReport(stdout, host.ID(), runtimes, report)

	fmt.Fprintf(stdout, "jig worker: claiming from %s\n", *serverURL)
	runWorkerLoops(ctx, hostCtx, host, *capacity, logger)
	return exitAccepted
}

// defaultServerAuthority is the loopback authority every client-side command
// defaults to — the same address `jig serve` binds (R20).
const defaultServerAuthority = "127.0.0.1:8383"

// runWorkerLoops runs the registration loop and one claim loop per capacity
// slot until the interrupt, then waits for in-flight attempts to unwind.
//
// signalCtx ends on SIGINT/SIGTERM and is what stops NEW work; host is the
// detached context every claim and completion runs on, so an attempt already
// under way finishes its unwind — the engine sees cancellation through the
// runner's own signal channel (workerAttemptRunner), not through a dead
// context.
func runWorkerLoops(
	signalCtx, host context.Context, w *worker.Worker, capacity int, logger *slog.Logger,
) {
	var loops sync.WaitGroup
	loops.Add(1)
	go func() {
		defer loops.Done()
		ticker := time.NewTicker(protocol.RegistrationInterval)
		defer ticker.Stop()
		for {
			select {
			case <-signalCtx.Done():
				return
			case <-ticker.C:
				if _, err := w.RegisterOnce(host); err != nil {
					logger.Warn("worker_registration_failed", "error", err)
				}
			}
		}
	}()
	for slot := 0; slot < capacity; slot++ {
		loops.Add(1)
		go func() {
			defer loops.Done()
			ticker := time.NewTicker(protocol.ClaimPollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-signalCtx.Done():
					return
				case <-ticker.C:
				}
				// ClaimOnce runs a claimed attempt to its terminal state
				// synchronously; capacity is enforced inside the worker, so
				// one goroutine per slot is exactly the concurrency the
				// registration advertises.
				if _, err := w.ClaimOnce(host); err != nil {
					logger.Warn("attempt_failed", "error", err)
				}
			}
		}()
	}

	<-signalCtx.Done()
	logger.Info("worker_stopping", "drain", workerDrainGrace.String())
	drained := make(chan struct{})
	go func() {
		loops.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(workerDrainGrace):
		logger.Warn("worker_drain_timeout",
			"detail", "an attempt did not unwind within the drain window; "+
				"the control plane will sweep it after its lease expires")
	}
}

// workerAttemptRunner is the engine at the AttemptRunner seam: one Runner per
// attempt, because the trace sink is per-attempt (KTD8) and so is the
// ephemeral scratch family (KTD11).
//
// The sink is the worker's own TraceStream (U8): it redacts the definition's
// sensitive env values before anything leaves the process, dual-writes the
// attempt-local JSONL raw record, and streams seq-ordered batches to the
// control plane so the UI can paint the run live.
func workerAttemptRunner(
	signalCtx context.Context, dataDir string, selected selectedRuntime,
	logger *slog.Logger, host func() *worker.Worker,
) worker.AttemptRunner {
	scratchRoot := filepath.Join(dataDir, "scratch")
	return worker.RunnerFunc(func(ctx context.Context, prepared *worker.PreparedAttempt) worker.Outcome {
		attemptID := prepared.Claim.Attempt.ID
		infraFail := func(err error) worker.Outcome {
			return worker.Outcome{State: protocol.AttemptFailed, Error: err.Error()}
		}
		// Belt to the engine's braces: the seeded credentials in the
		// ephemeral HOME die with the attempt on every exit path (KTD11).
		defer func() {
			if err := os.RemoveAll(filepath.Join(scratchRoot, attemptID)); err != nil {
				logger.Warn("attempt_scratch_destroy_failed", "attempt_id", attemptID, "error", err)
			}
		}()
		trace, err := host().OpenTrace(prepared)
		if err != nil {
			return infraFail(err)
		}
		// The drain runs on a detached context: an interrupted attempt still
		// has events worth delivering, and a dead context would drop the tail
		// of exactly the run an operator most wants to read.
		defer func() {
			if err := trace.Close(context.WithoutCancel(ctx)); err != nil {
				logger.Warn("attempt_trace_close_failed", "attempt_id", attemptID, "error", err)
			}
		}()

		runner, err := engine.New(engine.Config{
			Runtime:       selected.Runtime,
			Capability:    selected.Capability,
			Sink:          trace,
			ScratchRoot:   scratchRoot,
			SeedHome:      selected.Seeder,
			BaseEnv:       os.Environ(),
			RecordProcess: host().RecordProcessGroup,
			Logger:        logger,
		})
		if err != nil {
			return infraFail(err)
		}
		// Two cancellation sources, one channel: the control plane's cancel
		// (which rides the heartbeat response) and the operator's Ctrl-C. The
		// engine watches one channel, so they are merged rather than the
		// second one being dropped.
		return runner.Execute(context.WithoutCancel(ctx), engine.Attempt{
			Claim:        prepared.Claim,
			WorktreePath: prepared.WorktreePath,
			Branch:       prepared.Branch,
			BaseSHA:      prepared.BaseSHA,
			Cancelled:    anyClosed(signalCtx.Done(), prepared.Cancelled()),
			FreshenLease: prepared.FreshenLease,
		})
	})
}

// anyClosed merges cancellation channels: the result closes as soon as any
// input does. It never blocks the caller and never leaks past the first
// close, which is all the engine's one-shot cancellation signal needs.
func anyClosed(channels ...<-chan struct{}) <-chan struct{} {
	merged := make(chan struct{})
	var once sync.Once
	for _, channel := range channels {
		if channel == nil {
			continue
		}
		go func(c <-chan struct{}) {
			select {
			case <-c:
				once.Do(func() { close(merged) })
			case <-merged:
			}
		}(channel)
	}
	return merged
}

// printReconcileReport is the operator's startup picture: identity, probed
// runtime, what reconciliation found on disk, and — the publish half — every
// attempt-scoped branch on a remote that no fenced push record explains.
func printReconcileReport(
	stdout io.Writer, workerID string,
	runtimes []protocol.RuntimeCapability, report worker.PublishReconcileReport,
) {
	fmt.Fprintf(stdout, "jig worker: %s\n", workerID)
	for _, capability := range runtimes {
		fmt.Fprintf(stdout, "  runtime: %s %s (resume=%t, cost=%t)\n",
			capability.Name, capability.Version, capability.CanResume, capability.ReportsCost)
	}
	fmt.Fprintf(stdout, "  retained worktrees: %d, orphan paths: %d, missing: %d\n",
		len(report.Retained), len(report.OrphanPaths), len(report.MissingAttemptIDs))
	for _, path := range report.OrphanPaths {
		fmt.Fprintf(stdout, "  orphan worktree (reported, never deleted): %s\n", path)
	}
	for _, group := range report.StoppedProcessGroups {
		fmt.Fprintf(stdout, "  stopped orphaned agent process group: %d\n", group)
	}
	for _, stray := range report.StrayBranches {
		fmt.Fprintf(stdout, "  stray publish branch: %s %s (%s)\n",
			stray.Repository, stray.Branch, stray.Reason)
	}
}
