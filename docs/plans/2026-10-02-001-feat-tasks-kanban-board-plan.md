---
title: "feat: Kanban board layout for My tasks"
type: feat
date: 2026-10-02
---

# feat: Kanban board layout for My tasks

**Target repo:** `StructuPath/jig`. UI-only change under `web/`; no Go, API, or migration change.

---

## Summary

Add a read-only Kanban board as a second layout of the **My tasks** view (`/runs`). A `List | Board` switch picks the layout, remembered per browser. The board groups runs into five operator columns — **Waiting · Running · Needs review · Done · Stopped** — derived in the browser from data the UI already polls.

---

## Problem Frame

My tasks is a flat grid of task cards with a status badge each. With more than a handful of runs, an operator cannot see at a glance what is waiting for a worker, what is moving, and what needs a human. The run state alone does not say: `active` hides waiting vs running, and `mixed` covers both "held for review" and "partly failed".

---

## Requirements

- **R1.** My tasks offers a `List | Board` switch; List is the default and is unchanged.
- **R2.** The chosen layout persists per browser and survives disabled storage (falls back to List, never throws).
- **R3.** Board shows exactly five columns in order: Waiting, Running, Needs review, Done, Stopped — each with a count, and an empty-state line when it has no cards.
- **R4.** Every run appears in exactly one column; no run is dropped for an unrecognised state.
- **R5.** Cards reuse the existing task card and link to `/runs/{id}`. The board is read-only: no drag, no new actions.
- **R6.** The board updates live with the same polling primitives as the rest of the UI, and polls the queue only while the board is shown.
- **R7.** The board is usable at narrow widths (no page-level horizontal scroll).

---

## Key Technical Decisions

- **KTD1 — Frontend-only derivation.** Columns come from `api.runs()` plus `api.queue()`; no new endpoint or field. The queue already lists every `queued`/`active` job with its `run_id`, which is the only missing signal. *Rejected:* adding per-run job-state counts to `GET /api/runs` — cleaner single round-trip but touches `internal/controlplane` and `internal/protocol` for a layout change; deferred.
- **KTD2 — Column mapping** (directional; the pure fold owns it):

  | Column | Rule |
  |---|---|
  | Waiting | run `active` and every queue entry for it is job state `queued` |
  | Running | run `active` and any queue entry for it is job state `active`; also run `active` with **no** queue entry (poll skew between the two endpoints — the next poll corrects it) |
  | Needs review | `taskState(run)` is `ready` or `mixed`, or any state the fold does not recognise (R4 — an unknown state should reach a human, not vanish) |
  | Done | `taskState(run)` is `accepted` or `complete` |
  | Stopped | `taskState(run)` is `failed` or `cancelled` |

  `taskState` in `web/src/format.ts` is the single source for the held-publication `mixed → ready/complete` mapping; the board must call it rather than re-reading `run.state`.
- **KTD3 — Pure fold, thin component.** The mapping lives in a pure module so it is testable without a DOM, mirroring `web/src/lanes.ts` / `web/src/outcomes.ts`.
- **KTD4 — Layout switch in `localStorage`** under key `jig-tasks-layout`, read and written inside `try/catch` exactly like `storedProject()` in `web/src/Tasks.tsx`. Not a route or query param: it is a viewing preference, and deep links stay `/runs`.
- **KTD5 — No new dependency.** Plain CSS grid in `web/src/styles.css` using the existing tokens (`--panel`, `--line`, `--ok`, `--warn`, `--bad`). No drag-and-drop or board library.
- **KTD6 — Queue failure degrades, not blanks.** If the queue poll errors, the board still renders from runs (active runs fall to Running per KTD2) and shows the existing `ErrorBanner`.

---

## Implementation Units

### U1. Board fold

**Goal:** Pure function from runs + queue entries to five ordered columns.
**Requirements:** R3, R4
**Dependencies:** none
**Files:** create `web/src/board.ts`, `web/src/board.test.ts`
**Approach:** Export the column ids/labels in display order and a fold that takes `Run[]` and `QueueEntry[] | null` and returns each column with its runs. Index queue entries by `run_id` once. Preserve the incoming run order (API is newest first) within each column. Use `taskState` from `format.ts` for terminal states.
**Patterns to follow:** `web/src/lanes.ts` header comment style (why + plan ID), `web/src/outcomes.test.ts` for table-style cases.
**Test scenarios:**
- Happy path: one run per state — `active`+queued entry → Waiting; `active`+active entry → Running; `accepted` → Done; `failed` → Stopped; `cancelled` → Stopped.
- Multi-repo run `active` with one `queued` and one `active` entry → Running (any active wins).
- `active` run with no queue entry → Running (skew fallback).
- Queue is `null` (poll failed): `active` runs → Running, terminal runs unaffected.
- Held-publication single-target `mixed` run with ask mode → Done (`complete`); with build mode → Needs review (`ready`).
- Plain `mixed` multi-target run → Needs review.
- Unrecognised state (e.g. `"paused"`) → Needs review; total cards across columns equals input length.
- Empty runs → five empty columns in fixed order.
- Order within a column matches input order.
**Verification:** `board.test.ts` passes; every input run appears in exactly one column in every case.

### U2. Board view and layout switch

**Goal:** Render the board inside My tasks behind a persisted `List | Board` switch.
**Requirements:** R1, R2, R3, R5, R6, R7
**Dependencies:** U1
**Files:** create `web/src/TaskBoard.tsx`, `web/src/TaskBoard.test.tsx` (not `Board.tsx`: it collides with U1's `board.ts` on a case-insensitive filesystem, mirroring `lanes.ts`/`SwimLane.tsx`); modify `web/src/Queue.tsx` (`Runs`), `web/src/styles.css`; rebuild `web/dist/`
**Approach:** `Runs` keeps its runs poll and owns the layout state (KTD4). The switch is a two-button group in the view header with `aria-pressed`. List renders today's `task-cards` grid untouched. `TaskBoard` receives runs, starts its own `usePolled` on `api.queue()` (so the queue is polled only while mounted, R6), folds with U1, and renders five `section` columns each with a heading + count and `TaskCard`s; an empty column shows a muted one-liner. CSS: columns as a grid that collapses to a single stacked column at narrow width; the board container may scroll horizontally on mid widths but the page must not.
**Patterns to follow:** `Fleet.test.tsx` for mocking `api` and asserting with `within`; `TaskCard` in `web/src/Tasks.tsx`; guarded storage in `Tasks.tsx`.
**Test scenarios:**
- Default: no stored layout → List grid renders, no board columns, queue not requested.
- Click Board → five column headings in order with correct counts; card links point at `/runs/{id}`; layout written to storage.
- Stored `board` → board renders on first paint.
- `localStorage.getItem` throws → List renders, no crash; `setItem` throws on toggle → layout still switches in-session.
- Queue request rejects → board renders from runs with active runs under Running, error banner shown.
- Empty runs → existing "No tasks yet" empty state in both layouts (board does not render five empty columns for zero tasks).
- Switching back to List stops the queue poll (no further queue calls after unmount).
**Verification:** `npm run typecheck`, `npm test`, `npm run build` all clean, and the rebuilt `web/dist/` is committed (AGENTS.md rule; CI diffs it).

---

## Scope Boundaries

- No drag-and-drop, no cancel/readmit from the board.
- No Go, API, protocol, or migration change.
- No new nav item or route; `/runs` deep links unchanged.
- List layout and `TaskCard` markup unchanged.

### Deferred to Follow-Up Work

- Server-side per-run job-state summary on `GET /api/runs` (KTD1 alternative) if client derivation proves racy in practice.
- Filters (project, date) and column collapse.

---

## Risks

| Risk | Mitigation |
|---|---|
| Two independent polls disagree for one tick | KTD2 skew fallback to Running; next poll corrects; tested |
| `mixed` lumps partial failure with held review | Both genuinely need a human; Needs review is the honest column. Revisit with the deferred server summary |
| Bundle drift fails CI | U2 verification requires the rebuilt `web/dist/` in the same commit |
| Missing dependency passes locally via `~/node_modules` | No new imports beyond existing `web/package.json` deps (KTD5) |

---

## Verification (whole PR)

From `web/`: `npm run typecheck`, `npm test`, `npm run build` with `web/dist/` unchanged after rebuild. From repo root: `just check` (includes the UI embed test). Manual: `jig start`, open `/runs`, toggle Board with at least one waiting, running, and finished task — **not exercisable by CI; mark UNVERIFIED unless done in a browser.**
