---
name: webperf
description: Audit the jig web UI's performance — measure bundle size, render cost, and polling load before proposing any optimisation. Use when the UI feels slow, the bundle grows, or before adding a UI dependency.
argument-hint: "[view or concern, e.g. /runs board]"
---

# /webperf — measure before you optimize

The UI is a React app in `web/src/`, committed pre-built in `web/dist/` and
embedded in the Go binary. It talks to the local control plane by polling.
No optimisation is proposed without a before number, and none is kept
without an after number.

## Steps

1. **Baseline the bundle.** From `web/`: `npm run build`, then record the
   size of each file in `web/dist/assets/` raw and gzipped
   (`gzip -c <file> | wc -c`). Compare with `main`
   (`git stash`/worktree, rebuild, measure) if this branch changed the UI.
2. **Find what is in it.** List `dependencies` in `web/package.json` and
   their share of the bundle (e.g. a one-off
   `npx vite-bundle-visualizer`, not committed). Flag any new dependency;
   the UI deliberately keeps to React and plain CSS.
3. **Measure the runtime** with the app running (`jig start`, or the `run`
   skill) and Playwright/Chromium against it:
   - first load and time to interactive on the main views (`/runs`, run
     detail, fleet, queue);
   - polling: requests per minute per view from `web/src/polling.ts`, and
     whether hidden views or background tabs keep polling;
   - re-render cost with many runs (seed 100+ runs) — React Profiler or
     long-task timing.
4. **Rank findings by measured cost**, not by intuition. For each, state the
   number, the cause (file:line), and the proposed fix with its expected
   gain.
5. If asked to fix: change one thing, re-measure, and keep it only if the
   number moved. Then `npm run typecheck`, `npm test`, `npm run build`, and
   commit `web/dist/` with the source.

## Report

A table of metric → before → after (or target), then the ranked findings.
