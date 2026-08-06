// http.go — the control plane's HTTP surface (U2). JSON in, JSON out,
// bounded request bodies, and browser-origin fencing on every state-changing
// route (R20): a request carrying an Origin header must first present a
// trustworthy Host authority (loopback, never a resolved name — see
// hostAuthorityIsTrusted, which closes the DNS-rebinding hole where an
// attacker's domain resolves to 127.0.0.1 and Origin/Host agree with each
// other but not with reality), then either the server's own origin or the
// per-process UI token. Requests without an Origin header (the worker,
// curl) pass — the check exists to stop foreign web pages from driving a
// loopback control plane through the operator's browser, not to
// authenticate local clients.
package controlplane

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// API is the handler set over one Store.
type API struct {
	store   *Store
	uiToken string
	logger  *slog.Logger
}

// NewHandler builds the routing table. uiToken is the per-process token the
// embedded UI presents in lieu of a same-origin Origin header; empty
// disables token access.
func NewHandler(store *Store, uiToken string, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	api := &API{store: store, uiToken: uiToken, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", api.health)
	mux.HandleFunc("POST /api/definitions", api.createDefinition)
	mux.HandleFunc("GET /api/definitions", api.listDefinitions)
	mux.HandleFunc("GET /api/definitions/{definition_id}", api.getDefinition)
	mux.HandleFunc("PUT /api/definitions/{definition_id}", api.updateDefinition)
	mux.HandleFunc("POST /api/runs", api.invokeDefinition)
	mux.HandleFunc("GET /api/runs", api.listRuns)
	mux.HandleFunc("GET /api/runs/{run_id}", api.getRun)
	mux.HandleFunc("POST /api/runs/{run_id}/readmit", api.readmitRun)
	mux.HandleFunc("PUT /api/workers/{worker_id}", api.registerWorker)
	mux.HandleFunc("POST /api/workers/{worker_id}/claims", api.claim)
	mux.HandleFunc("POST /api/attempts/{attempt_id}/start", api.startAttempt)
	mux.HandleFunc("PUT /api/attempts/{attempt_id}/heartbeat", api.heartbeat)
	mux.HandleFunc("POST /api/attempts/{attempt_id}/complete", api.completeAttempt)
	mux.HandleFunc("POST /api/jobs/{job_id}/retry", api.retryJob)
	mux.HandleFunc("POST /api/jobs/{job_id}/cancel", api.cancelJob)
	mux.HandleFunc("GET /api/workers/{worker_id}/worktrees", api.workerWorktrees)
	mux.HandleFunc("POST /api/workers/{worker_id}/worktrees/reconcile", api.reconcileWorktrees)
	mux.HandleFunc("POST /api/worktrees/{attempt_id}/release", api.releaseWorktree)
	// Publish routes register themselves from publish_ledger.go (U7), where
	// their handlers live beside the fenced ledger they call.
	api.registerPublishRoutes(mux)
	return mux
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	if err := a.store.db.PingContext(r.Context()); err != nil {
		writeError(w, unavailable(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- definitions and runs (U5) -------------------------------------------
//
// Authoring and invocation are operator surfaces, so they take the same
// mutation gate as every worker route: prepareMutation first, always (R20).

func (a *API) createDefinition(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input DefinitionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	definition, err := a.store.CreateDefinition(r.Context(), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, definition)
}

func (a *API) updateDefinition(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input DefinitionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	definition, err := a.store.UpdateDefinition(r.Context(), r.PathValue("definition_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, definition)
}

func (a *API) getDefinition(w http.ResponseWriter, r *http.Request) {
	definition, err := a.store.Definition(r.Context(), r.PathValue("definition_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, definition)
}

func (a *API) listDefinitions(w http.ResponseWriter, r *http.Request) {
	definitions, err := a.store.Definitions(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, definitions)
}

func (a *API) invokeDefinition(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input RunInvocation
	if !decodeJSON(w, r, &input) {
		return
	}
	view, err := a.store.InvokeDefinition(r.Context(), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// readmitRun is the KTD9 escape hatch's route: a bodyless POST, because
// re-admitting takes nothing but the run whose pins are stale.
func (a *API) readmitRun(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	view, err := a.store.ReadmitRunAtHead(r.Context(), r.PathValue("run_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (a *API) getRun(w http.ResponseWriter, r *http.Request) {
	view, err := a.store.RunDetail(r.Context(), r.PathValue("run_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := a.store.Runs(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (a *API) registerWorker(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.WorkerRegistration
	if !decodeJSON(w, r, &input) {
		return
	}
	worker, err := a.store.RegisterWorker(r.Context(), r.PathValue("worker_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

func (a *API) claim(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.ClaimRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	claim, err := a.store.Claim(r.Context(), r.PathValue("worker_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	if claim == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, claim)
}

func (a *API) startAttempt(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.StartAttemptRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	attempt, err := a.store.StartAttempt(r.Context(), r.PathValue("attempt_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attempt)
}

func (a *API) heartbeat(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.HeartbeatRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	response, err := a.store.Heartbeat(r.Context(), r.PathValue("attempt_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *API) completeAttempt(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.CompleteAttemptRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	attempt, err := a.store.CompleteAttempt(r.Context(), r.PathValue("attempt_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attempt)
}

func (a *API) retryJob(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	job, err := a.store.RetryJob(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (a *API) cancelJob(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	job, err := a.store.CancelJob(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (a *API) workerWorktrees(w http.ResponseWriter, r *http.Request) {
	entries, err := a.store.WorkerWorktrees(r.Context(), r.PathValue("worker_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (a *API) reconcileWorktrees(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.WorktreeReconciliationReport
	if !decodeJSON(w, r, &input) {
		return
	}
	workerID := r.PathValue("worker_id")
	result, err := a.store.ReconcileWorktrees(r.Context(), workerID, input)
	if err != nil {
		writeError(w, err)
		return
	}
	// Orphans are worker disk with no manifest: nobody deletes them, the
	// operator learns about them here (R16).
	for _, orphan := range input.OrphanPaths {
		a.logger.Warn("orphan_worktree_reported", "worker_id", workerID, "path", orphan)
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) releaseWorktree(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.WorktreeReleaseRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	entry, err := a.store.ReleaseWorktree(r.Context(), r.PathValue("attempt_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// prepareMutation gates every state-changing route (R20): a trusted Host
// authority, Origin fencing, and a bounded request body. It never
// authenticates originless clients — loopback binding is the perimeter;
// these checks close the browser hole in it.
func (a *API) prepareMutation(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && !a.uiTokenPresented(r) {
		// r.Host is attacker-controlled under DNS rebinding: a page served
		// from evil.test, whose name the attacker points at 127.0.0.1,
		// makes the browser send both Origin and Host as "evil.test:port".
		// sameAuthority below would agree they match and wave the request
		// through same-origin. Reject on the Host authority itself first —
		// it must name loopback, never an attacker's domain — before
		// trusting any comparison against it.
		if !hostAuthorityIsTrusted(r) {
			writeError(w, &ServiceError{Code: "untrusted_host_authority",
				Message: "state-changing requests must target a loopback host", Status: 403})
			return false
		}
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme != "http" || !sameAuthority(parsed.Host, r.Host) {
			writeError(w, &ServiceError{Code: "cross_origin_request",
				Message: "state-changing requests must be same-origin or carry the UI token", Status: 403})
			return false
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, protocol.MaxRequestBodyBytes)
	return true
}

func (a *API) uiTokenPresented(r *http.Request) bool {
	presented := r.Header.Get("X-Jig-UI-Token")
	return a.uiToken != "" && presented != "" &&
		subtle.ConstantTimeCompare([]byte(presented), []byte(a.uiToken)) == 1
}

func sameAuthority(left, right string) bool {
	return strings.EqualFold(strings.TrimSuffix(left, "."), strings.TrimSuffix(right, "."))
}

// hostAuthorityIsTrusted reports whether r.Host names loopback rather than
// an attacker's DNS-rebound domain (R20). The hostname must be a loopback
// IP literal (127.0.0.0/8, ::1) or "localhost" — never resolved, so a
// rebound name can never satisfy it by any DNS trick. When Host carries a
// port and the server's own listener port is known from the connection
// (net/http sets LocalAddrContextKey per accepted connection; direct
// ServeHTTP calls in tests do not), that port must match too, closing the
// door on a same-machine service masquerading on a different loopback port.
// A portless Host is accepted on the hostname check alone.
func hostAuthorityIsTrusted(r *http.Request) bool {
	host, port, hasPort := splitHostAuthority(r.Host)
	if !isLoopbackHostname(host) {
		return false
	}
	if !hasPort {
		return true
	}
	if expected, ok := listenerPort(r); ok && expected != port {
		return false
	}
	return true
}

// splitHostAuthority splits a Host header value into hostname and port.
// Unlike net.SplitHostPort it tolerates a portless authority ("127.0.0.1",
// "[::1]", "localhost") by falling back to the whole value with brackets
// trimmed.
func splitHostAuthority(hostHeader string) (host, port string, hasPort bool) {
	if h, p, err := net.SplitHostPort(hostHeader); err == nil {
		return h, p, true
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostHeader, "["), "]"), "", false
}

// isLoopbackHostname reports whether host is a loopback IP literal or the
// literal name "localhost". It never resolves — resolution is exactly the
// step DNS rebinding subverts.
func isLoopbackHostname(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// listenerPort reports the port the server actually accepted this
// connection on, when known.
func listenerPort(r *http.Request) (string, bool) {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return "", false
	}
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", false
	}
	return port, true
}

// decodeJSON reads exactly one JSON value with unknown fields rejected. A
// missing or empty body is allowed only when target is nil (bodyless routes
// pass nil and never call this).
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writeError(w, &ServiceError{Code: "json_required",
			Message: "Content-Type must be application/json", Status: 415})
		return false
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeDecodeError(w, err)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("request body must contain a single JSON value")
		}
		writeDecodeError(w, err)
		return false
	}
	return true
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, &ServiceError{Code: "request_too_large",
			Message: "request body exceeds its limit", Status: 413})
		return
	}
	writeError(w, &ServiceError{Code: "invalid_json",
		Message: "request body is not valid JSON for this route: " + err.Error(), Status: 400})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	var service *ServiceError
	if !errors.As(err, &service) {
		service = &ServiceError{Code: "internal_error", Message: "internal error", Status: 500}
	}
	writeJSON(w, service.Status, map[string]any{
		"error": map[string]string{"code": service.Code, "message": service.Message},
	})
}
