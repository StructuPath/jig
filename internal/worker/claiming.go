// claiming.go — the claim loop, worker-minted lease tokens, and per-attempt
// heartbeating (U3). One lease token per claim, minted here and never stored
// server-side except as a digest. Every attempt gets a heartbeat goroutine
// at HeartbeatInterval; after any possible gap (machine sleep), the ordering
// is always heartbeat first — only a heartbeat may revive an
// expired-but-unswept lease (R5) — and start/complete run behind freshen.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// mintLeaseToken mints one fencing token per claim: 32 random bytes,
// hex-encoded to 64 — inside the protocol's 32–1024 byte contract.
func mintLeaseToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint lease token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// attemptLease owns one attempt's lease client-side: renewal bookkeeping,
// the background heartbeat, and the cancellation signal that rides the
// heartbeat response (R5).
type attemptLease struct {
	client    *Client
	attemptID string
	token     string
	now       func() time.Time

	mutex       sync.Mutex
	lastRenewal time.Time

	cancelled  chan struct{}
	cancelOnce sync.Once

	// lost is the control plane's verdict that this lease can never renew
	// again, once any heartbeat has received one.
	lost error
}

// lostVerdict reports the lease-lost verdict a heartbeat received, or nil.
func (l *attemptLease) lostVerdict() error {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return l.lost
}

func newAttemptLease(client *Client, attemptID, token string) *attemptLease {
	return &attemptLease{
		client:    client,
		attemptID: attemptID,
		token:     token,
		now:       time.Now,
		cancelled: make(chan struct{}),
		// The claim transaction just granted a fresh lease.
		lastRenewal: time.Now(),
	}
}

// heartbeat performs one renewal and observes cancellation.
func (l *attemptLease) heartbeat(ctx context.Context) error {
	response, err := l.client.Heartbeat(ctx, l.attemptID, protocol.HeartbeatRequest{LeaseToken: l.token})
	if err != nil {
		if leaseLost(err) {
			l.mutex.Lock()
			l.lost = err
			l.mutex.Unlock()
		}
		return err
	}
	l.mutex.Lock()
	l.lastRenewal = l.now()
	l.mutex.Unlock()
	if response.CancellationRequested {
		l.cancelOnce.Do(func() { close(l.cancelled) })
	}
	return nil
}

// freshen is the wake-safe ordering primitive: if the last successful
// renewal is more than one HeartbeatInterval old — a delayed tick, a
// machine sleep — heartbeat before anything else, because start/complete
// require an unexpired lease while heartbeat alone may revive an
// expired-but-unswept one (R5).
func (l *attemptLease) freshen(ctx context.Context) error {
	l.mutex.Lock()
	stale := l.now().Sub(l.lastRenewal) >= heartbeatInterval
	l.mutex.Unlock()
	if !stale {
		return nil
	}
	return l.heartbeat(ctx)
}

// lost reports whether an error is a control-plane verdict that this lease
// can never renew again. Transport errors are uncertainty, not verdicts.
func leaseLost(err error) bool {
	switch errorCode(err) {
	case "lease_not_owner", "lease_superseded", "attempt_transitioned":
		return true
	}
	return false
}

// heartbeatInterval is protocol.HeartbeatInterval; a variable so tests can
// make a heartbeat land inside a window they control.
var heartbeatInterval = protocol.HeartbeatInterval

// keepAlive heartbeats at HeartbeatInterval until ctx ends or the lease is
// verdict-lost. Transient failures keep trying: the next beat may land, and
// an expired-but-unswept lease is still renewable (R5).
func (l *attemptLease) keepAlive(ctx context.Context, logger interface {
	Warn(msg string, args ...any)
}) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.heartbeat(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				logger.Warn("attempt_heartbeat_failed", "attempt_id", l.attemptID, "error", err)
				if leaseLost(err) {
					return
				}
			}
		}
	}
}

// ClaimOnce makes one claim request and, when work arrives, runs the whole
// attempt to its terminal state synchronously: prepare, start, run, complete,
// dispose-or-retain. A nil, nil answer means nothing was eligible.
func (w *Worker) ClaimOnce(ctx context.Context) (*protocol.Attempt, error) {
	if w.activeCount() >= w.config.Capacity {
		return nil, nil
	}
	claim, token, err := w.claim(ctx)
	if err != nil || claim == nil {
		return nil, err
	}
	w.trackActive(claim.Attempt.ID)
	defer w.untrackActive(claim.Attempt.ID)
	attempt, err := w.runAttempt(ctx, claim, token)
	if err != nil {
		return nil, err
	}
	return attempt, nil
}

// claimResendDelays spaces the resends of one claim request after a
// transport failure; a variable so tests need not wait.
var claimResendDelays = []time.Duration{500 * time.Millisecond, time.Second}

// claim makes one claim request under a freshly minted token and request id.
// A transport failure leaves it unknown whether the server committed the
// claim, so the same (request_id, token) pair is resent before anything new
// is minted: the server replays a committed claim for that pair (R4), and a
// new pair would strand the committed attempt preparing under a token no
// worker holds until the sweeper calls it lost.
func (w *Worker) claim(ctx context.Context) (*protocol.Claim, string, error) {
	token, err := mintLeaseToken()
	if err != nil {
		return nil, "", err
	}
	requestID, err := newUUID()
	if err != nil {
		return nil, "", err
	}
	request := protocol.ClaimRequest{RequestID: requestID, LeaseToken: token}
	claim, err := w.client.Claim(ctx, request)
	for _, delay := range claimResendDelays {
		if err == nil || !retryable(err) {
			break
		}
		if waitErr := sleepContext(ctx, delay); waitErr != nil {
			return nil, "", err
		}
		claim, err = w.client.Claim(ctx, request)
	}
	if err != nil || claim == nil {
		return nil, "", err
	}
	return claim, token, nil
}

// retryable reports whether a request may be resent unchanged: a transport
// error or a 5xx answer is uncertainty about whether the server acted, while
// any 4xx answer is a verdict that repeating cannot change.
func retryable(err error) bool {
	var apiError *APIError
	if errors.As(err, &apiError) {
		return apiError.Status >= 500
	}
	return true
}

// sleepContext waits for delay or until ctx ends, whichever comes first.
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// runClaimLoop polls for work while capacity is free, running each claimed
// attempt in its own goroutine up to the capacity slot count.
func (w *Worker) runClaimLoop(ctx context.Context) {
	var attempts sync.WaitGroup
	defer attempts.Wait()
	ticker := time.NewTicker(protocol.ClaimPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Requests come before claims: a person is waiting on a release, and
		// its work is already done.
		w.servePublishRequests(ctx, &attempts)
		for w.activeCount() < w.config.Capacity {
			claim, token, err := w.claim(ctx)
			if err != nil {
				if ctx.Err() == nil {
					w.logger.Warn("claim_failed", "error", err)
				}
				break
			}
			if claim == nil {
				break
			}
			w.trackActive(claim.Attempt.ID)
			attempts.Add(1)
			go func(claim *protocol.Claim, token string) {
				defer attempts.Done()
				defer w.untrackActive(claim.Attempt.ID)
				if _, err := w.runAttempt(ctx, claim, token); err != nil && ctx.Err() == nil {
					w.logger.Warn("attempt_failed", "attempt_id", claim.Attempt.ID, "error", err)
				}
			}(claim, token)
		}
	}
}

// runAttempt drives one claimed attempt to its terminal state. Every fenced
// control-plane write goes through freshen first (wake-safe ordering, R5),
// and disposal afterward fails closed regardless of how the attempt ended.
func (w *Worker) runAttempt(ctx context.Context, claim *protocol.Claim, token string) (*protocol.Attempt, error) {
	lease := newAttemptLease(w.client, claim.Attempt.ID, token)
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go lease.keepAlive(heartbeatCtx, w.logger)

	prepared, prepErr := w.prepareAttempt(ctx, claim, lease)
	if prepErr != nil {
		outcome := Outcome{State: protocol.AttemptFailed,
			Error: boundedText("prepare attempt: "+prepErr.Error(), protocol.MaxErrorBytes)}
		attempt, completeErr := w.completeAttempt(ctx, lease, outcome)
		if completeErr != nil {
			return nil, errors.Join(prepErr, completeErr)
		}
		return &attempt, nil
	}

	if err := lease.freshen(ctx); err != nil {
		return nil, fmt.Errorf("freshen lease before start: %w", err)
	}
	runtimeName, runtimeVersion := w.primaryRuntime()
	if _, err := w.client.StartAttempt(ctx, claim.Attempt.ID, protocol.StartAttemptRequest{
		LeaseToken: token, RuntimeName: runtimeName, RuntimeVersion: runtimeVersion,
	}); err != nil {
		w.retainAfterAttempt(ctx, claim.Attempt.ID,
			"attempt could not start: "+err.Error())
		return nil, fmt.Errorf("start attempt: %w", err)
	}
	if _, err := w.manifests.update(claim.Attempt.ID, func(manifest *attemptManifest) error {
		manifest.Lifecycle = manifestRunning
		return nil
	}); err != nil {
		// The attempt is already running server-side. Abandoning it here would
		// leave the ledger row running until the sweeper called it lost, so the
		// terminal state is reported before returning, and the worktree is
		// retained because a manifest we cannot write is a manifest disposal
		// cannot trust.
		outcome := Outcome{State: protocol.AttemptFailed,
			Error: boundedText("record running lifecycle: "+err.Error(), protocol.MaxErrorBytes)}
		if _, completeErr := w.completeAttempt(ctx, lease, outcome); completeErr != nil {
			err = errors.Join(err, completeErr)
		}
		w.retainAfterAttempt(ctx, claim.Attempt.ID,
			"attempt manifest could not record the running lifecycle: "+err.Error())
		return nil, err
	}

	outcome := w.config.Runner.Run(ctx, prepared)
	// The runner has used whatever it was going to; a continuation still
	// held here would only pin the attempt's scratch directory.
	releaseContinuation(outcome)
	if _, err := w.manifests.update(claim.Attempt.ID, func(manifest *attemptManifest) error {
		manifest.Lifecycle = manifestCompleted
		manifest.TerminalState = outcome.State
		return nil
	}); err != nil {
		// The runner finished and its outcome exists: report it rather than
		// discard it, then retain, because the manifest no longer describes
		// what happened and disposal reasons from the manifest.
		w.logger.Warn("attempt_manifest_completion_failed",
			"attempt_id", claim.Attempt.ID, "error", err)
		if _, completeErr := w.completeAttempt(ctx, lease, outcome); completeErr != nil {
			err = errors.Join(err, completeErr)
		}
		w.retainAfterAttempt(ctx, claim.Attempt.ID,
			"attempt manifest could not record completion: "+err.Error())
		return nil, err
	}
	attempt, err := w.completeAttempt(ctx, lease, outcome)
	if err != nil {
		// The outcome could not be recorded (swept, superseded, server down):
		// the work's value is unknown, so the worktree is retained, never
		// deleted on uncertainty.
		w.retainAfterAttempt(ctx, claim.Attempt.ID,
			"attempt outcome could not be recorded: "+err.Error())
		return nil, err
	}
	stopHeartbeat()
	if err := w.disposeAttemptWorktree(ctx, claim.Attempt.ID); err != nil {
		w.logger.Warn("attempt_cleanup_failed", "attempt_id", claim.Attempt.ID, "error", err)
	}
	return &attempt, nil
}

// completionRetryDelays spaces the retries of one completion after a
// transport failure or a 5xx answer, about two minutes in all; a variable so
// tests need not wait.
var completionRetryDelays = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
	15 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
}

// completeAttempt reports the terminal outcome, retrying while the outcome's
// fate is uncertain. The caller's heartbeat keeps renewing the lease during
// the retries, so a timeout, a busy database, or a server restart does not
// turn finished work — possibly an already-merged PR — into a swept-lost
// attempt. Repeating is safe because the control plane replays a terminal
// completion for the owning token and keeps the first stored outcome (R6).
// Only a verdict (a 4xx answer, including a lost lease) or a cancelled
// context ends the retries early.
//
// One heartbeat verdict is not a reason to stop: attempt_transitioned is
// exactly what a heartbeat sees after the completion it is retrying has
// committed with its answer lost, so the retry is sent anyway and the
// server's replay (or its lease_not_owner) decides (R6).
func (w *Worker) completeAttempt(ctx context.Context, lease *attemptLease, outcome Outcome) (protocol.Attempt, error) {
	request := protocol.CompleteAttemptRequest{
		LeaseToken: lease.token,
		State:      outcome.State,
		Result:     boundedText(outcome.Result, protocol.MaxResultBytes),
		Error:      boundedText(outcome.Error, protocol.MaxErrorBytes),
		// The hold travels as a field of its own, not only inside the result
		// summary: the control plane records it and never parses a result.
		PublishHold: boundedText(outcome.PublishHold, protocol.MaxPublishDiagnosticBytes),
	}
	attempt, err := w.completeAttemptOnce(ctx, lease, request)
	for _, delay := range completionRetryDelays {
		if err == nil || !retryable(err) || !completionMayRetry(lease.lostVerdict()) {
			break
		}
		w.logger.Warn("attempt_completion_retry", "attempt_id", lease.attemptID, "error", err)
		if waitErr := sleepContext(ctx, delay); waitErr != nil {
			break
		}
		attempt, err = w.completeAttemptOnce(ctx, lease, request)
	}
	return attempt, err
}

func (w *Worker) completeAttemptOnce(ctx context.Context, lease *attemptLease, request protocol.CompleteAttemptRequest) (protocol.Attempt, error) {
	if err := lease.freshen(ctx); err != nil && errorCode(err) != "attempt_transitioned" {
		return protocol.Attempt{}, fmt.Errorf("freshen lease before completion: %w", err)
	}
	return w.client.CompleteAttempt(ctx, lease.attemptID, request)
}

// completionMayRetry reports whether the lease verdict a heartbeat recorded
// (nil when none) still leaves the completion worth sending.
// attempt_transitioned means the attempt is no longer leased — possibly
// because this very completion already committed — and only the completion
// route can tell a replay from a sweep. lease_not_owner and
// lease_superseded are final.
func completionMayRetry(verdict error) bool {
	return verdict == nil || errorCode(verdict) == "attempt_transitioned"
}

// primaryRuntime names the first probed runtime for the attempt record
// (KTD4); U4's engine picks per role.
func (w *Worker) primaryRuntime() (string, string) {
	w.stateMutex.Lock()
	defer w.stateMutex.Unlock()
	if len(w.runtimes) == 0 {
		return "", ""
	}
	return w.runtimes[0].Name, w.runtimes[0].Version
}
