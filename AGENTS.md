# jig — agent instructions

jig is a local-first software factory: one Go binary (control plane, worker,
CLI) plus a React UI that is committed pre-built and embedded. Read
`README.md` for the model (definition → run → job → attempt → phases) and
`docs/plans/` for the design records (KTD/R/U numbers cited in code and
commits).

## Layout

| Path | What |
|---|---|
| `cmd/jig/` | CLI entry points (`start`, `serve`, `worker`, `run`, `def`, `trigger`, `report`) |
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

## Workflow

Project slash commands live in `.claude/skills/`. A change moves left to
right; each stage hands its output to the next.

```
 DEFINE          PLAN           BUILD          VERIFY         REVIEW          SHIP
 ┌──────┐      ┌──────┐      ┌──────┐      ┌──────┐      ┌──────┐      ┌──────┐
 │ Idea │ ───▶ │ Spec │ ───▶ │ Code │ ───▶ │ Test │ ───▶ │  QA  │ ───▶ │  Go  │
 │Refine│      │  PRD │      │ Impl │      │Debug │      │ Gate │      │ Live │
 └──────┘      └──────┘      └──────┘      └──────┘      └──────┘      └──────┘
  /spec          /plan          /build        /test         /review       /ship
```

| Command | Does | Key principle |
|---|---|---|
| `/spec` | Starts a `docs/plans/` record: problem frame, `R` requirements, scope | Spec before code |
| `/plan` | Adds `KTD` decisions, `U` units, risks, verification to that record | Small, atomic tasks |
| `/build` | Builds one `U` test-first and commits it citing its IDs | One slice at a time |
| `/test` | Maps every `R` to a test that can fail, fills gaps, runs the CI suite | Tests are proof |
| `/review` | Blocking vs optional findings with evidence, against jig's invariants | Improve code health |
| `/ship` | CI-equivalent checks, push the branch, open the PR — never `main` | Faster is safer |

Use these at any stage:

| Command | Does | Key principle |
|---|---|---|
| `/constraints` | Turns a decision into a rule here plus a check that enforces it | Decide it once, enforce it everywhere |
| `/code-simplify` | Removes duplication and dead code without changing behaviour; the tests stay green | Clarity over cleverness |
| `/webperf` | Measures bundle size, render cost, and polling for `web/` before any optimisation | Measure before you optimize |

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
- **Parallel-group members are read-only (apart from declared
  `build_outputs`) and concurrent.** Anything that
  dispatches or replays phases must not run a member outside the group.
- Work on a branch in a sibling worktree, open a PR, never push to `main`.
- `gofmt` everything; match the surrounding comment style, which explains
  why and cites the plan ID.
