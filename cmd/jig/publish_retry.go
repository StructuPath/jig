// publish_retry.go — `jig worker publish-retry` (R14): the operator's
// publish-only retry, run as the worker that owns the attempt, plus the
// data-directory lock it shares with `jig worker`.
//
// The retry is a one-shot over the worker's persisted identity and data
// directory. It deliberately skips everything `jig worker` does to be able to
// RUN attempts — runtime selection, the probe, registration, startup
// reconciliation — because a publish-only retry runs no phase: it needs no
// agent CLI and no agent auth, and registering would overwrite the running
// worker's advertised capabilities with an empty set. What it does need is
// the attempt's retained worktree, manifest, and repository cache, which
// only the owning worker's data directory holds — and which nothing else may
// be writing at the same time. That is what the lock is for.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/worker"
)

const workerPublishRetryUsage = `Usage: jig worker publish-retry [flags] <job-id>

Finish publishing an accepted_unpublished job — a held release, a pull
request that failed to open, CI that was red — without re-running any
phase. The SAME attempt is re-leased and only the publish steps the ledger
does not already prove are run: the same branch, the same pull request, CI
judged on the branch's current head. No agent starts and no inference is
spent; CI repair rounds and CI re-runs never happen on a retry.

Run it as the worker that ran the attempt: the same --data directory, and
the same --commit-author-name/--commit-author-email/--base-branch that
worker uses. Stop that worker first — the data directory is locked while
either one runs.

Flags:
  --server <url>         control plane base URL (default http://127.0.0.1:8383)
  --data <dir>           the worker data directory the attempt ran in
                         (default ~/.jig/worker)
  --commit-author-name   author name on jig's publish commit
                         (default "jig")
  --commit-author-email  author email on jig's publish commit
                         (default "jig@localhost")
  --base-branch          pull-request base branch; empty resolves the
                         repository's default branch

Exit codes:
  0  published: the attempt is accepted
  1  the retry could not run: the data directory is in use, the control
     plane is unreachable, or it refused the retry (another worker's
     attempt, a job that is not accepted_unpublished, an earlier retry
     still holding its lease)
  2  usage error
  3  the retry ran but publish still did not finish; the job stays
     accepted_unpublished and can be retried again
`

// newPublishGateway builds the GitHub gateway publish runs through. A
// variable so the command tests can substitute a fake GitHub, exactly as the
// worker-wiring tests hand NewPublishingRunner one.
var newPublishGateway = func() worker.PullRequestGateway { return worker.NewGitHubCLIGateway() }

func workerPublishRetryCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jig worker publish-retry", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, workerPublishRetryUsage) }
	serverURL := flags.String("server", "http://"+defaultServerAuthority, "control plane URL")
	dataDir := flags.String("data", defaultWorkerDataDir(), "worker data directory")
	authorName := flags.String("commit-author-name", "jig", "publish commit author name")
	authorEmail := flags.String("commit-author-email", "jig@localhost", "publish commit author email")
	baseBranch := flags.String("base-branch", "", "pull-request base branch")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != 1 || strings.TrimSpace(flags.Arg(0)) == "" || *dataDir == "" {
		flags.Usage()
		return exitUsage
	}
	jobID := flags.Arg(0)
	if ctx == nil {
		ctx = context.Background()
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "jig worker publish-retry: %v\n", err)
		return exitInfraFailed
	}
	lock, err := lockWorkerData(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "jig worker publish-retry: %v\n", err)
		return exitInfraFailed
	}
	defer lock.release()

	host, err := worker.New(worker.Config{
		ServerURL:     *serverURL,
		DataDir:       *dataDir,
		WorkerVersion: version,
		// No probe runs: a publish-only retry starts no agent.
		Probes: []worker.RuntimeProbe{},
		Logger: commandLogger(stderr),
	})
	if err != nil {
		fmt.Fprintf(stderr, "jig worker publish-retry: %v\n", err)
		return exitInfraFailed
	}
	// The retry runs to its recorded end on a context the interrupt does not
	// cancel, as a worker's attempts do: a publish cut off after its push but
	// before its record is exactly the state the retry exists to finish. A
	// retry killed outright stays leased until the sweeper returns it to
	// accepted_unpublished.
	attempt, err := host.RetryPublish(context.WithoutCancel(ctx), jobID, newPublishGateway(),
		worker.PublishOptions{
			CommitAuthorName:  *authorName,
			CommitAuthorEmail: *authorEmail,
			BaseBranch:        *baseBranch,
		})
	if err != nil {
		fmt.Fprintf(stderr, "jig worker publish-retry: %v\n", err)
		var refused *worker.APIError
		if errors.As(err, &refused) && refused.Code == "publish_retry_foreign_worker" {
			fmt.Fprintf(stderr, "jig worker publish-retry: %s is worker %s; run the retry with the --data "+
				"directory of the worker that ran the attempt\n", *dataDir, host.ID())
		}
		return exitInfraFailed
	}
	printPublishRetry(stdout, attempt)
	if attempt.State != protocol.AttemptAccepted {
		return exitRejected
	}
	return exitAccepted
}

// printPublishRetry is the retry's answer: the attempt (the same one), its
// state, and where publish got to.
func printPublishRetry(stdout io.Writer, attempt protocol.Attempt) {
	var document struct {
		Publish worker.PublishSummary `json:"publish"`
	}
	_ = json.Unmarshal([]byte(attempt.Result), &document)
	summary := document.Publish
	fmt.Fprintf(stdout, "attempt: %s\n", attempt.ID)
	fmt.Fprintf(stdout, "attempt_number: %d\n", attempt.AttemptNumber)
	fmt.Fprintf(stdout, "state: %s\n", attempt.State)
	fmt.Fprintf(stdout, "branch: %s\n", protocol.PublishBranch(attempt.JobID, attempt.AttemptNumber))
	if summary.PullRequestURL != "" {
		fmt.Fprintf(stdout, "pull_request: %s\n", summary.PullRequestURL)
	}
	if summary.State != "" {
		fmt.Fprintf(stdout, "publish: %s\n", summary.State)
	}
	if summary.Code != "" {
		fmt.Fprintf(stdout, "publish_code: %s\n", summary.Code)
	}
	if summary.Detail != "" {
		fmt.Fprintf(stdout, "publish_detail: %s\n", summary.Detail)
	}
}

func defaultWorkerDataDir() string {
	if base := defaultDataDir(); base != "" {
		return filepath.Join(base, "worker")
	}
	return ""
}

// ---- the data-directory lock -----------------------------------------------

// workerDataLock is the exclusive flock on <data>/worker.lock that `jig
// worker` and `jig worker publish-retry` hold for their whole lifetime. One
// data directory is one worker identity with one set of manifests, worktrees,
// and repository cache entries; two processes over it would race on all of
// them — a retry disposing the worktree a running worker's reconciliation is
// reading, or two publishers on one attempt branch.
//
// The lock lives on the open file description, so the kernel drops it
// however the process ends, SIGKILL included: there is no pid to check and
// no stale lock to clear. And it lives on the file, not the path: any alias
// of the directory — a symlink, a relative path — reaches the same inode and
// the same lock.
type workerDataLock struct{ file *os.File }

func lockWorkerData(dataDir string) (*workerDataLock, error) {
	path := filepath.Join(dataDir, "worker.lock")
	// The lock file is written (the pid note below), so it must be the data
	// directory's own: a worker.lock that is a symlink or a hard link to some
	// other file would have that file truncated. O_NOFOLLOW refuses a symlink
	// at open — a dangling one included, which O_CREAT would otherwise create
	// the target of — and the link count, read from the open file, refuses a
	// hard link. Both are refused, never repaired: the operator decides.
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("worker data lock %s is a symbolic link; jig refuses to follow it — "+
			"remove it and jig will create the lock file again", path)
	}
	if err != nil {
		return nil, fmt.Errorf("open worker data lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := requireOwnLockFile(file, path); err != nil {
		file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("the worker data directory %s is in use by another jig worker process "+
				"(it holds %s); stop that `jig worker` or `jig worker publish-retry` first", dataDir, path)
		}
		return nil, fmt.Errorf("lock worker data directory %s: %w", dataDir, err)
	}
	// The pid is a note for an operator reading the file, never consulted.
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	}
	return &workerDataLock{file: file}, nil
}

// requireOwnLockFile checks the OPEN file, so what is checked is what gets
// locked and written: a regular file with exactly one name.
func requireOwnLockFile(file *os.File, path string) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect worker data lock: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("worker data lock %s is not a regular file; remove it and jig will create it again", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return fmt.Errorf("worker data lock %s has more than one name (a hard link); jig refuses to write "+
			"through it — remove it and jig will create the lock file again", path)
	}
	return nil
}

// release drops the lock. The file stays: removing it would let a racing
// opener lock an inode nobody else can find.
func (l *workerDataLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
}
