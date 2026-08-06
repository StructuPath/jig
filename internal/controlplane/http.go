// http.go — the control plane's HTTP surface (U2). JSON in, JSON out,
// bounded request bodies, and browser-origin fencing on every state-changing
// route (R20): a request carrying an Origin header must present either the
// server's own origin or the per-process UI token. Requests without an
// Origin header (the worker, curl) pass — the check exists to stop foreign
// web pages from driving a loopback control plane through the operator's
// browser, not to authenticate local clients.
package controlplane

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
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
	mux.HandleFunc("PUT /api/workers/{worker_id}", api.registerWorker)
	mux.HandleFunc("POST /api/workers/{worker_id}/claims", api.claim)
	mux.HandleFunc("POST /api/attempts/{attempt_id}/start", api.startAttempt)
	mux.HandleFunc("PUT /api/attempts/{attempt_id}/heartbeat", api.heartbeat)
	mux.HandleFunc("POST /api/attempts/{attempt_id}/complete", api.completeAttempt)
	mux.HandleFunc("POST /api/jobs/{job_id}/retry", api.retryJob)
	mux.HandleFunc("POST /api/jobs/{job_id}/cancel", api.cancelJob)
	return mux
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	if err := a.store.db.PingContext(r.Context()); err != nil {
		writeError(w, unavailable(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

// prepareMutation gates every state-changing route (R20): Origin fencing
// plus a bounded request body. It never authenticates originless clients —
// loopback binding is the perimeter; this check closes the browser hole in
// it.
func (a *API) prepareMutation(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && !a.uiTokenPresented(r) {
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
