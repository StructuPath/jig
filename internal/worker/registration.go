// registration.go — worker identity, configuration, the runtime probe seam,
// and the registration loop (U3, KTD12). The single implicit worker owns a
// data directory; its identity is a UUID persisted there, so restarts keep
// the same worker and the same manifests. Registration advertises env-var
// names only (never values, R17), probed runtime capabilities (KTD4), and
// the current retained-worktree set (R16). Registration is also the idle
// liveness heartbeat: claims never refresh liveness, so the loop runs every
// RegistrationInterval whether or not work exists.
package worker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

var (
	uuidPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	versionPattern = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)+[0-9A-Za-z.+-]*`)
)

// RuntimeProbe detects one installed agent CLI at worker start (KTD4). U4's
// runtime adapters implement this interface with richer capability
// detection; U3 ships the version-string CommandProbe.
type RuntimeProbe interface {
	Probe(ctx context.Context) (protocol.RuntimeCapability, error)
}

// CommandProbe probes a CLI by running it with version arguments and
// extracting the version token from its first output line. The capability
// flags are declared, not detected — U3 knows them per CLI; U4's adapters
// own detecting them properly.
type CommandProbe struct {
	Name        string
	Executable  string
	Args        []string
	CanResume   bool
	ReportsCost bool
}

// Probe runs the CLI and reports its capability record, or an error when the
// CLI is not installed or emits nothing version-shaped.
func (p CommandProbe) Probe(ctx context.Context) (protocol.RuntimeCapability, error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.GitCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, p.Executable, p.Args...)
	output, err := command.Output()
	if err != nil {
		return protocol.RuntimeCapability{}, fmt.Errorf("probe %s: %w", p.Executable, err)
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	version := versionPattern.FindString(firstLine)
	if version == "" {
		return protocol.RuntimeCapability{}, fmt.Errorf(
			"probe %s: no version in output %q", p.Executable, firstLine)
	}
	return protocol.RuntimeCapability{
		Name: p.Name, Version: version,
		CanResume: p.CanResume, ReportsCost: p.ReportsCost,
	}, nil
}

// DefaultProbes is the stock probe set: the Claude Code CLI (the one v1
// runtime, KTD4). Codex joins in U10.
func DefaultProbes() []RuntimeProbe {
	return []RuntimeProbe{
		CommandProbe{Name: "claude-code", Executable: "claude", Args: []string{"--version"},
			CanResume: true, ReportsCost: true},
	}
}

// Outcome is what an attempt runner declares when it finishes. State must be
// a worker-declarable terminal attempt state; success is earned, so the zero
// value is not a valid outcome.
type Outcome struct {
	State  string
	Result string
	Error  string
}

// AttemptRunner executes one prepared attempt. U4's phase engine implements
// this seam; U3 without an engine fails every attempt explicitly.
type AttemptRunner interface {
	Run(ctx context.Context, attempt *PreparedAttempt) Outcome
}

// RunnerFunc adapts a function to AttemptRunner.
type RunnerFunc func(ctx context.Context, attempt *PreparedAttempt) Outcome

// Run implements AttemptRunner.
func (f RunnerFunc) Run(ctx context.Context, attempt *PreparedAttempt) Outcome {
	return f(ctx, attempt)
}

// noEngineRunner is the U3 default: no phase engine exists yet, so an
// attempt that reaches execution fails loudly instead of pretending.
type noEngineRunner struct{}

func (noEngineRunner) Run(context.Context, *PreparedAttempt) Outcome {
	return Outcome{State: protocol.AttemptFailed, Error: "no phase engine is wired (U4)"}
}

// RecordProcessGroup records an attempt's live subprocess group into its
// manifest (U4). The phase engine reports each agent process group as it
// starts and stops — the manifest's ProcessGroupID/ProcessActive fields are
// what start-time reconciliation uses to stop orphaned groups a crashed
// worker left behind.
func (w *Worker) RecordProcessGroup(attemptID string, processGroupID int64, active bool) error {
	_, err := w.manifests.update(attemptID, func(manifest *attemptManifest) error {
		manifest.ProcessGroupID = processGroupID
		manifest.ProcessActive = active
		return nil
	})
	return err
}

// Config configures the single implicit worker.
type Config struct {
	// ServerURL is the control plane's http://host:port address.
	ServerURL string
	// DataDir is the worker-owned state root: worker-id, attempts/ (manifests
	// + disposal journal), repos/ (managed cache), worktrees/.
	DataDir string
	// Name is the human worker name; empty means "local".
	Name string
	// WorkerVersion is reported in registration; empty means "dev".
	WorkerVersion string
	// Capacity is the concurrent-attempt slot count; zero means 1.
	Capacity int
	// Environ is the composed worker environment ("NAME=value" entries) whose
	// names are advertised at registration (R17). Nil means os.Environ().
	Environ []string
	// Probes detect installed agent CLIs (KTD4). Nil means DefaultProbes().
	Probes []RuntimeProbe
	// Runner executes prepared attempts (the U4 seam). Nil fails every
	// attempt with an explicit no-engine error.
	Runner AttemptRunner
	Logger *slog.Logger
}

// Worker is the single implicit worker (KTD12).
type Worker struct {
	config    Config
	id        string
	client    *Client
	manifests *manifestStore
	cache     *repoCache
	logger    *slog.Logger

	stateMutex sync.Mutex
	active     map[string]bool
	retained   map[string]protocol.RetainedWorktree
	runtimes   []protocol.RuntimeCapability
	environ    []string
}

// New builds a worker over its data directory, creating the directory and a
// persistent worker identity on first start.
func New(config Config) (*Worker, error) {
	if config.DataDir == "" {
		return nil, errors.New("worker data directory is required")
	}
	if config.Name == "" {
		config.Name = "local"
	}
	if config.WorkerVersion == "" {
		config.WorkerVersion = "dev"
	}
	if config.Capacity == 0 {
		config.Capacity = 1
	}
	if config.Capacity < 1 || config.Capacity > 100 {
		return nil, errors.New("worker capacity must be between 1 and 100")
	}
	if config.Runner == nil {
		config.Runner = noEngineRunner{}
	}
	if config.Probes == nil {
		config.Probes = DefaultProbes()
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	environ := config.Environ
	if environ == nil {
		environ = os.Environ()
	}
	if err := os.MkdirAll(config.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create worker data directory: %w", err)
	}
	dataDir, err := filepath.EvalSymlinks(config.DataDir)
	if err != nil {
		return nil, fmt.Errorf("canonicalize worker data directory: %w", err)
	}
	config.DataDir = dataDir
	workerID, err := loadOrCreateWorkerID(dataDir)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(config.ServerURL, workerID)
	if err != nil {
		return nil, err
	}
	return &Worker{
		config:    config,
		id:        workerID,
		client:    client,
		manifests: newManifestStore(dataDir, workerID),
		cache:     newRepoCache(filepath.Join(dataDir, "repos")),
		logger:    config.Logger,
		active:    make(map[string]bool),
		retained:  make(map[string]protocol.RetainedWorktree),
		environ:   environ,
	}, nil
}

// ID is the worker's stable identity.
func (w *Worker) ID() string { return w.id }

// SetEnviron replaces the composed worker environment; the next registration
// advertises the new name set (R17).
func (w *Worker) SetEnviron(environ []string) {
	w.stateMutex.Lock()
	defer w.stateMutex.Unlock()
	w.environ = append([]string(nil), environ...)
}

// loadOrCreateWorkerID reads the persistent identity from the data
// directory, minting one on first start. A stable identity is what lets
// restarts reclaim their manifests and ledger rows (KTD12).
func loadOrCreateWorkerID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "worker-id")
	body, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(body))
		if !uuidPattern.MatchString(id) {
			return "", fmt.Errorf("worker identity file %s is corrupt", path)
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read worker identity: %w", err)
	}
	id, err := newUUID()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("persist worker identity: %w", err)
	}
	return id, nil
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ProbeRuntimes runs every configured probe and records the detected agent
// CLIs for registration (KTD4). A CLI that fails its probe is absent, not
// fatal: a worker with no runtimes can still register and reconcile.
func (w *Worker) ProbeRuntimes(ctx context.Context) []protocol.RuntimeCapability {
	var runtimes []protocol.RuntimeCapability
	for _, probe := range w.config.Probes {
		capability, err := probe.Probe(ctx)
		if err != nil {
			w.logger.Info("runtime_probe_missed", "error", err)
			continue
		}
		runtimes = append(runtimes, capability)
	}
	w.stateMutex.Lock()
	w.runtimes = runtimes
	w.stateMutex.Unlock()
	return runtimes
}

// registration snapshots the worker's current advertisement: capacity,
// active slots, env-var names from the composed environment (names only,
// R17), probed runtimes, and the retained-worktree set (R16).
func (w *Worker) registration() protocol.WorkerRegistration {
	w.stateMutex.Lock()
	defer w.stateMutex.Unlock()
	retained := make([]protocol.RetainedWorktree, 0, len(w.retained))
	for _, entry := range w.retained {
		retained = append(retained, entry)
	}
	sort.Slice(retained, func(i, j int) bool { return retained[i].AttemptID < retained[j].AttemptID })
	return protocol.WorkerRegistration{
		Name:              w.config.Name,
		WorkerVersion:     w.config.WorkerVersion,
		Capacity:          w.config.Capacity,
		ActiveCount:       len(w.active),
		EnvNames:          envNamesFromEnviron(w.environ),
		Runtimes:          append([]protocol.RuntimeCapability(nil), w.runtimes...),
		RetainedWorktrees: retained,
	}
}

// envNamesFromEnviron extracts the name half of every entry — values never
// leave the process (R17).
func envNamesFromEnviron(environ []string) []string {
	seen := make(map[string]bool, len(environ))
	names := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, found := strings.Cut(entry, "=")
		if !found || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RegisterOnce sends one registration upsert; the answer is the control
// plane's stored view of this worker.
func (w *Worker) RegisterOnce(ctx context.Context) (protocol.Worker, error) {
	return w.client.Register(ctx, w.registration())
}

// runRegistrationLoop re-registers every RegistrationInterval until ctx
// ends, keeping an idle worker live for claim eligibility (KTD12).
func (w *Worker) runRegistrationLoop(ctx context.Context) {
	ticker := time.NewTicker(protocol.RegistrationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.RegisterOnce(ctx); err != nil && ctx.Err() == nil {
				w.logger.Warn("worker_registration_failed", "error", err)
			}
		}
	}
}

// Run is the worker main loop: probe, register, reconcile disk against the
// ledger (U3), then keep registering and claiming until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	w.ProbeRuntimes(ctx)
	if _, err := w.RegisterOnce(ctx); err != nil {
		return fmt.Errorf("initial registration: %w", err)
	}
	if _, err := w.Reconcile(ctx); err != nil {
		return fmt.Errorf("startup reconciliation: %w", err)
	}
	go w.runRegistrationLoop(ctx)
	w.runClaimLoop(ctx)
	return ctx.Err()
}

func (w *Worker) activeCount() int {
	w.stateMutex.Lock()
	defer w.stateMutex.Unlock()
	return len(w.active)
}

func (w *Worker) trackActive(attemptID string) {
	w.stateMutex.Lock()
	defer w.stateMutex.Unlock()
	w.active[attemptID] = true
}

func (w *Worker) untrackActive(attemptID string) {
	w.stateMutex.Lock()
	defer w.stateMutex.Unlock()
	delete(w.active, attemptID)
}

func (w *Worker) recordRetained(entry protocol.RetainedWorktree) {
	w.stateMutex.Lock()
	defer w.stateMutex.Unlock()
	w.retained[entry.AttemptID] = entry
}

func (w *Worker) forgetRetained(attemptID string) {
	w.stateMutex.Lock()
	defer w.stateMutex.Unlock()
	delete(w.retained, attemptID)
}

func (w *Worker) worktreeRoot() string {
	return filepath.Join(w.config.DataDir, "worktrees")
}
