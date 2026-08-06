// Package worker holds the jig worker: registration, claiming, repo cache,
// worktrees, manifests, and reconciliation (U3); the phase engine plugs into
// the AttemptRunner seam (U4) and publish follows (U7).
//
// Boundary invariant (KTD1): this package must never import
// internal/controlplane — the worker speaks to the control plane over HTTP
// only, so a later process split stays cheap. The Justfile `boundary` recipe
// enforces this mechanically via `go list -deps`.
package worker
