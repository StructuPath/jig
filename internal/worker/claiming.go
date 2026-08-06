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

	"github.com/StructuPath/jig/internal/protocol"
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
	stale := l.now().Sub(l.lastRenewal) >= protocol.HeartbeatInterval
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

// keepAlive heartbeats at HeartbeatInterval until ctx ends or the lease is
// verdict-lost. Transient failures keep trying: the next beat may land, and
// an expired-but-unswept lease is still renewable (R5).
func (l *attemptLease) keepAlive(ctx context.Context, logger interface {
	Warn(msg string, args ...any)
}) {
	ticker := time.NewTicker(protocol.HeartbeatInterval)
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
	token, err := mintLeaseToken()
	if err != nil {
		return nil, err
	}
	requestID, err := newUUID()
	if err != nil {
		return nil, err
	}
	claim, err := w.client.Claim(ctx, protocol.ClaimRequest{RequestID: requestID, LeaseToken: token})
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
		for w.activeCount() < w.config.Capacity {
			token, err := mintLeaseToken()
			if err != nil {
				w.logger.Warn("claim_failed", "error", err)
				break
			}
			requestID, err := newUUID()
			if err != nil {
				w.logger.Warn("claim_failed", "error", err)
				break
			}
			claim, err := w.client.Claim(ctx, protocol.ClaimRequest{RequestID: requestID, LeaseToken: token})
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

func (w *Worker) completeAttempt(ctx context.Context, lease *attemptLease, outcome Outcome) (protocol.Attempt, error) {
	if err := lease.freshen(ctx); err != nil {
		return protocol.Attempt{}, fmt.Errorf("freshen lease before completion: %w", err)
	}
	return w.client.CompleteAttempt(ctx, lease.attemptID, protocol.CompleteAttemptRequest{
		LeaseToken: lease.token,
		State:      outcome.State,
		Result:     boundedText(outcome.Result, protocol.MaxResultBytes),
		Error:      boundedText(outcome.Error, protocol.MaxErrorBytes),
	})
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
