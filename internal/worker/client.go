// client.go — the worker's HTTP client for the control plane (U3). The
// worker speaks to the server only through this client (KTD1): shared types
// come from internal/protocol, transport is loopback HTTP, and every request
// is bounded by WorkerRequestTimeout.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// APIError is a control-plane error answer: the stable code the worker
// branches on (lease_not_owner, lease_superseded, attempt_transitioned,
// claim_conflict, claim_request_conflict) plus the HTTP status.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("control plane rejected the request (%d %s): %s", e.Status, e.Code, e.Message)
}

// Client talks to one control plane on behalf of one worker.
type Client struct {
	baseURL  string
	workerID string
	http     *http.Client
}

// NewClient validates the server URL and binds the worker identity every
// worker-scoped route needs.
func NewClient(serverURL, workerID string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSuffix(serverURL, "/"))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return nil, fmt.Errorf("server URL must be an http://host:port address, got %q", serverURL)
	}
	if strings.TrimSpace(workerID) == "" {
		return nil, fmt.Errorf("worker id is required")
	}
	return &Client{
		baseURL:  parsed.String(),
		workerID: workerID,
		http:     &http.Client{Timeout: protocol.WorkerRequestTimeout},
	}, nil
}

// Register upserts the worker (PUT /api/workers/{id}); the answer is the
// control plane's view of the worker, including the advertised env names.
func (c *Client) Register(ctx context.Context, registration protocol.WorkerRegistration) (protocol.Worker, error) {
	var worker protocol.Worker
	err := c.call(ctx, http.MethodPut, "/api/workers/"+url.PathEscape(c.workerID), registration, &worker)
	return worker, err
}

// Claim requests work (POST /api/workers/{id}/claims). A nil, nil answer is
// an empty claim: nothing eligible right now.
func (c *Client) Claim(ctx context.Context, request protocol.ClaimRequest) (*protocol.Claim, error) {
	var claim protocol.Claim
	found, err := c.callOptional(ctx, http.MethodPost,
		"/api/workers/"+url.PathEscape(c.workerID)+"/claims", request, &claim)
	if err != nil || !found {
		return nil, err
	}
	return &claim, nil
}

// StartAttempt moves a claimed attempt preparing → running (fenced, R6).
func (c *Client) StartAttempt(ctx context.Context, attemptID string, request protocol.StartAttemptRequest) (protocol.Attempt, error) {
	var attempt protocol.Attempt
	err := c.call(ctx, http.MethodPost, "/api/attempts/"+url.PathEscape(attemptID)+"/start", request, &attempt)
	return attempt, err
}

// Heartbeat renews the attempt lease and carries cancellation back (R5).
func (c *Client) Heartbeat(ctx context.Context, attemptID string, request protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	var response protocol.HeartbeatResponse
	err := c.call(ctx, http.MethodPut, "/api/attempts/"+url.PathEscape(attemptID)+"/heartbeat", request, &response)
	return response, err
}

// CompleteAttempt records the attempt's terminal outcome (fenced, R6).
func (c *Client) CompleteAttempt(ctx context.Context, attemptID string, request protocol.CompleteAttemptRequest) (protocol.Attempt, error) {
	var attempt protocol.Attempt
	err := c.call(ctx, http.MethodPost, "/api/attempts/"+url.PathEscape(attemptID)+"/complete", request, &attempt)
	return attempt, err
}

// Worktrees reads this worker's retained-worktree ledger rows (R16).
func (c *Client) Worktrees(ctx context.Context) ([]protocol.WorktreeLedgerEntry, error) {
	var entries []protocol.WorktreeLedgerEntry
	err := c.call(ctx, http.MethodGet, "/api/workers/"+url.PathEscape(c.workerID)+"/worktrees", nil, &entries)
	return entries, err
}

// ReconcileWorktrees sends the worker's disk truth and receives the applied
// ledger view (R16).
func (c *Client) ReconcileWorktrees(ctx context.Context, report protocol.WorktreeReconciliationReport) (protocol.WorktreeReconciliationResult, error) {
	var result protocol.WorktreeReconciliationResult
	err := c.call(ctx, http.MethodPost,
		"/api/workers/"+url.PathEscape(c.workerID)+"/worktrees/reconcile", report, &result)
	return result, err
}

func (c *Client) call(ctx context.Context, method, path string, body, target any) error {
	found, err := c.callOptional(ctx, method, path, body, target)
	if err == nil && !found {
		return &APIError{Status: http.StatusNoContent, Code: "empty_response",
			Message: "the control plane answered with no content where content was required"}
	}
	return err
}

// callOptional performs one request; a 204 answer returns (false, nil).
func (c *Client) callOptional(ctx context.Context, method, path string, body, target any) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.WorkerRequestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return false, fmt.Errorf("encode %s %s request: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return false, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, protocol.MaxRequestBodyBytes))
	if err != nil {
		return false, fmt.Errorf("read %s %s response: %w", method, path, err)
	}
	if response.StatusCode == http.StatusNoContent {
		return false, nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return false, decodeAPIError(response.StatusCode, payload)
	}
	if target != nil {
		if err := json.Unmarshal(payload, target); err != nil {
			return false, fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return true, nil
}

func decodeAPIError(status int, payload []byte) error {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Error.Code == "" {
		return &APIError{Status: status, Code: "unexpected_response",
			Message: fmt.Sprintf("non-JSON error answer (%d bytes)", len(payload))}
	}
	return &APIError{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

// errorCode extracts the stable control-plane code, or "" for transport
// errors — the distinction that drives fail-closed decisions: a code is a
// verdict, a transport error is uncertainty.
func errorCode(err error) string {
	var apiError *APIError
	if errors.As(err, &apiError) {
		return apiError.Code
	}
	return ""
}
