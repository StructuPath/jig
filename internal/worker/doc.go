// Package worker will hold the jig worker: registration, claiming, repo
// cache, worktrees, manifests, and publish (U3, U7).
//
// Boundary invariant (KTD1): this package must never import
// internal/controlplane — the worker speaks to the control plane over HTTP
// only, so a later process split stays cheap. The Justfile `boundary` recipe
// enforces this mechanically via `go list -deps`.
package worker
