# jig — agent instructions

jig is a local-first software factory: one Go binary (control plane, worker,
CLI) plus a React UI that is committed pre-built and embedded. Read
`README.md` for the model (definition → run → job → attempt → phases) and
`docs/plans/` for the design records (KTD/R/U numbers cited in code and
commits).

## Layout

| Path | What |
|---|---|
| `cmd/jig/` | CLI entry points (`serve`, `worker`, `run`, `def`, `trigger`, `report`) |
| `internal/protocol/` | Definition schema and save-time validation |
| `internal/engine/` | Phase engine: chain, gates, repair edges, parallel group |
| `internal/controlplane/` | HTTP API, SQLite ledger, queue, reports |
| `internal/worker/` | Claims jobs, runs attempts, publishes via `gh` |
| `internal/runtime/` | Agent CLI adapters (`claudecode`, `codex`) |
| `migrations/` | SQLite migrations |
| `web/src/` | UI source; `web/dist/` is the committed build |
| `examples/definitions/` | Stock definitions, validated in CI |

## Commands

Go (from the repo root):

| Command | Runs |
|---|---|
| `just check` | format-check, vet, boundary, definitions, test, build — what CI runs |
| `just test-race` | Race suite: controlplane, worker, engine, runtime |
| `just test` | `go test -timeout 5m ./...` |
| `go test ./internal/engine/ -run <Name>` | One test while iterating |

UI (from `web/`):

| Command | Runs |
|---|---|
| `npm run typecheck` | `tsc -b` over app and vite config |
| `npm test` | vitest |
| `npm run build` | Rebuilds `web/dist/` |

## Rules

- **Commit the rebuilt bundle.** Any change under `web/src/` needs
  `npm run build` and the resulting `web/dist/` committed; CI fails if the
  rebuild differs from the tree.
- **Every module the UI imports must be in `web/package.json`.** This Mac has
  a `~/node_modules` (including `@types/node`) that tsc and Node fall back
  to, so a missing dependency passes locally and fails in CI.
- **The worker never imports the control plane** (`just boundary`, KTD1).
- **Definition changes are validated at save time.** A new definition field
  or rule goes in `internal/protocol/definition.go` with a test, and the stock
  definitions must still pass `just definitions`.
- **Parallel-group members are read-only and concurrent.** Anything that
  dispatches or replays phases must not run a member outside the group.
- Work on a branch in a sibling worktree, open a PR, never push to `main`.
- `gofmt` everything; match the surrounding comment style, which explains
  why and cites the plan ID.
