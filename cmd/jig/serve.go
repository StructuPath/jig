// serve.go — `jig serve` (U9): the whole control plane in one command. It
// opens the store (which applies the embedded migrations), recovers
// interrupted admissions BEFORE serving anything, then runs the API, the
// embedded UI, the lease sweeper, and the admission loop until the operator
// stops it.
//
// Recovery ordering is not incidental. Admission commits an occurrence and
// then dispatches it (R13); a crash between those two steps leaves the row in
// `dispatching`, and nothing in the periodic tick moves it back — only
// AdmissionRunner.Recover does. Serving first and recovering later would
// leave that occurrence stranded for as long as the server lives, so Recover
// runs first and a failure refuses to start: a control plane that could not
// complete its recovery pass has not established the invariant it serves
// under.
//
// The listener lives here rather than in controlplane.Server because the
// embedded UI and the API must answer on ONE origin — a UI on a second port
// would fail the API's own Origin check on every state-changing request
// (R20). controlplane.Server has no seam for a second handler, so serve
// composes the mux itself and borrows the server's construction as the
// authority on the bind rule: the loopback refusal has exactly one
// implementation and this is not a second copy of it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/web"
)

const serveUsage = `Usage: jig serve [--data <dir>] [--addr <host:port>] [--allow-non-loopback] [--no-github-poll]

Start the control plane: the HTTP API, the embedded UI, the lease sweeper,
and the admission loop (schedules and GitHub polling), over one SQLite
database. Migrations apply on open.

Flags:
  --data <dir>           data directory holding jig.db (default ~/.jig)
  --addr <host:port>     listen address (default 127.0.0.1:8383)
  --allow-non-loopback   bind a non-loopback address; jig refuses to
                         without this flag, because it has no
                         authentication and one trusted operator (R20)
  --no-github-poll       do not poll GitHub triggers; schedules keep
                         firing. Use it when ` + "`gh`" + ` is absent or
                         unauthenticated.

Exit codes:
  0  the server shut down cleanly on SIGINT/SIGTERM
  1  infrastructure failure — the server could not start or serve
  2  usage error, including a refused non-loopback bind
`

// serveShutdownGrace bounds the drain after the operator interrupts: in-flight
// requests finish, then the listener closes regardless.
const serveShutdownGrace = 10 * time.Second

func serveCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jig serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, serveUsage) }
	dataDir := flags.String("data", defaultDataDir(), "data directory")
	address := flags.String("addr", controlplane.DefaultListenAddress, "listen address")
	allowNonLoopback := flags.Bool("allow-non-loopback", false, "bind a non-loopback address")
	noGitHubPoll := flags.Bool("no-github-poll", false, "disable GitHub trigger polling")
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
		fmt.Fprintf(stderr, "jig serve: %v\n", err)
		return exitInfraFailed
	}
	// Open applies the embedded migrations and records each in the ledger; a
	// re-open is a no-op (U1).
	store, err := controlplane.Open(ctx, filepath.Join(*dataDir, "jig.db"))
	if err != nil {
		fmt.Fprintf(stderr, "jig serve: %v\n", err)
		return exitInfraFailed
	}
	defer store.Close()

	// The bind rule (R20) is enforced by constructing the control-plane
	// server: it refuses a non-loopback address before any socket exists.
	// serve does not start it — it takes the refusal and the per-process UI
	// token, and owns the listener itself so the UI and the API share one
	// origin.
	plane, err := controlplane.NewServer(ctx, store, controlplane.ServerConfig{
		Address:          *address,
		AllowNonLoopback: *allowNonLoopback,
		Logger:           logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "jig serve: %v\n", err)
		return exitUsage
	}

	// A nil gateway disables GitHub polling while leaving schedules working
	// (the AdmissionRunner contract): a machine with no `gh` is still a
	// working scheduler.
	var gateway controlplane.GitHubGateway
	if !*noGitHubPoll {
		gateway = controlplane.NewGitHubCLIGateway()
	}
	admission := controlplane.NewAdmissionRunner(store, gateway, logger)
	if err := admission.Recover(ctx); err != nil {
		fmt.Fprintf(stderr,
			"jig serve: admission recovery failed, refusing to serve: %v\n", err)
		return exitInfraFailed
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *address)
	if err != nil {
		fmt.Fprintf(stderr, "jig serve: listen on %s: %v\n", *address, err)
		return exitInfraFailed
	}
	server := &http.Server{
		Handler:           serveHandler(store, plane.UIToken(), logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	// Background work outlives the interrupt only as long as the drain does:
	// the context is detached from the signal so an in-flight request is not
	// answered by a store whose sweeper vanished mid-transaction, and it is
	// cancelled explicitly once the drain is over.
	background, stopBackground := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBackground()
	sweeper := controlplane.NewSweeper(store)
	go sweeper.Run(background, logger)
	go admission.Run(background)

	serveFailed := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveFailed <- err
			return
		}
		serveFailed <- nil
	}()
	fmt.Fprintf(stdout, "jig serve: listening on http://%s\n", listener.Addr())

	select {
	case err := <-serveFailed:
		if err != nil {
			fmt.Fprintf(stderr, "jig serve: %v\n", err)
			return exitInfraFailed
		}
		return exitAccepted
	case <-ctx.Done():
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(
		context.WithoutCancel(ctx), serveShutdownGrace)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(stderr, "jig serve: shutdown: %v\n", err)
		return exitInfraFailed
	}
	return exitAccepted
}

// serveHandler composes the two halves of the one origin: the control-plane
// API under /api/, and the embedded UI (U8) on everything else. Both are
// reachable at the same host and port, which is what lets the UI's
// state-changing requests pass the API's Origin check as same-origin (R20)
// without carrying the per-process token at all.
func serveHandler(store *controlplane.Store, uiToken string, logger *slog.Logger) http.Handler {
	api := controlplane.NewHandler(store, uiToken, logger)
	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	mux.Handle("/", web.Handler())
	return mux
}

// defaultDataDir is the shared default for every command that owns state:
// ~/.jig, or empty when the home directory cannot be resolved (which makes
// --data mandatory rather than making jig guess).
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".jig")
}

// commandLogger is the structured logger long-running commands write to.
// stderr, so stdout stays the command's own output.
func commandLogger(stderr io.Writer) *slog.Logger {
	level := slog.LevelInfo
	if strings.EqualFold(strings.TrimSpace(os.Getenv("JIG_LOG_LEVEL")), "debug") {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
}
