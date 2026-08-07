// fleet.go — the worker fleet read surface (U8's read-only half, extended).
//
// Every other worker route addresses ONE worker by id: registration, claims,
// the per-worker worktree ledger. None of them answers the question an
// operator asks first when nothing is moving — which workers exist, which of
// them the claim transaction still counts live, and how much capacity is
// free. That question has exactly one correct answer and the control plane is
// the only place that holds it, so it is computed here rather than assembled
// by the caller.
//
// Liveness is not a second opinion: it is workerIsLive (claim.go), the claim
// transaction's own predicate, evaluated against the server's clock. A UI
// that derived it from last_heartbeat would be reading the BROWSER's clock,
// and a skewed laptop would show a ready worker the queue is skipping.
//
// Following the publish ledger's pattern: the store method and the handler
// live together, with one route line in http.go.
package controlplane

import (
	"context"
	"net/http"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// FleetMember is one registered worker as an operator reads it: the stored
// record plus the two facts only the control plane can state — whether this
// worker is still claim-eligible, and how many attempt slots it has free.
type FleetMember struct {
	protocol.Worker
	Live      bool `json:"live"`
	Available int  `json:"available"`
}

// FleetView is the fleet with the counters that answer "can anything run
// right now". AvailableSlots counts live workers only: capacity behind a
// stale worker is not capacity, and an operator who adds it up gets a queue
// that never drains for reasons the number denies.
//
// Stale workers are listed, never hidden. A worker that stopped heartbeating
// is usually the whole explanation for a queue that is not moving, and its
// leased attempts are what the sweeper is about to mark lost (R5).
type FleetView struct {
	Workers        []FleetMember `json:"workers"`
	LiveCount      int           `json:"live_count"`
	StaleCount     int           `json:"stale_count"`
	AvailableSlots int           `json:"available_slots"`
	ObservedAt     time.Time     `json:"observed_at"`
}

// Fleet lists every registered worker in name order (id breaks the tie), so
// a polling view holds its row order while workers heartbeat underneath it.
func (s *Store) Fleet(ctx context.Context) (FleetView, error) {
	now := s.now()
	view := FleetView{Workers: []FleetMember{}, ObservedAt: now.UTC()}
	rows, err := s.db.QueryContext(ctx, workerSelect+` ORDER BY name, id`)
	if err != nil {
		return view, unavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		worker, err := scanWorker(rows)
		if err != nil {
			return view, err
		}
		member := FleetMember{
			Worker:    worker,
			Live:      workerIsLive(now, worker.LastHeartbeat),
			Available: max(worker.Capacity-worker.ActiveCount, 0),
		}
		if member.Live {
			view.LiveCount++
			view.AvailableSlots += member.Available
		} else {
			view.StaleCount++
		}
		view.Workers = append(view.Workers, member)
	}
	if err := rows.Err(); err != nil {
		return view, unavailable(err)
	}
	return view, nil
}

// listWorkers is read-only, so it takes no mutation gate — the same rule the
// rest of the read surface follows (R20 fences DRIVING the control plane from
// a foreign page, not reading a loopback server the operator already trusts).
func (a *API) listWorkers(w http.ResponseWriter, r *http.Request) {
	fleet, err := a.store.Fleet(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fleet)
}
