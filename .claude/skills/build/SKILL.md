---
name: build
description: Build one implementation unit from a docs/plans/ record — write the test first, implement the smallest change, run its checks, and commit it citing the U/R IDs. Use to implement planned work one slice at a time.
argument-hint: "[U-number] [path to plan record]"
---

# /build — one slice at a time

Build exactly one implementation unit, prove it, commit it, then stop.

Pick the unit named in `$ARGUMENTS`, or the first unit in the newest
`docs/plans/` record that has no commit citing it yet (`git log --grep 'U<n>'`).
If there is no plan, stop and suggest `/spec` then `/plan`.

## Steps

1. **Be on a branch.** Never commit to `main`. If on `main`, create a branch
   (in a sibling worktree on this Mac, per `AGENTS.md`).
2. **Re-read the unit and the files it names.** If the code no longer
   matches the plan, stop and update the plan rather than improvising.
3. **Write the failing test first** from the unit's test scenarios. Run it
   and see it fail for the expected reason.
4. **Implement the smallest change that passes.** Match the surrounding code;
   comments explain *why* and cite the plan ID (`// KTD3: …`, `(U2)`).
   Respect the layer rules:
   - `internal/worker` never imports `internal/controlplane` (KTD1).
   - A new definition field or rule goes in
     `internal/protocol/definition.go` with a test.
   - Anything that dispatches or replays phases must not run a
     parallel-group member outside its group.
   - Every module the UI imports is listed in `web/package.json`.
5. **Run the unit's verification** plus the fast checks for what changed:
   - Go: `gofmt -l cmd internal migrations` is empty, `go vet ./...`, the
     package tests.
   - UI: `npm run typecheck`, `npm test`, then `npm run build` and stage the
     rebuilt `web/dist/`.
   - Definitions: `just definitions`.
6. **Commit** the unit alone, conventional style, citing IDs:
   `feat(engine): retry repair edges on transient failure (U2, R3)`.
7. **Report** what was built, the commands run and their results, and the
   next unit. Do not start it unless asked.

## Never

- Build more than one unit per invocation.
- Skip, weaken, or delete a test to get green.
- Leave `web/src/` changed without the matching `web/dist/` rebuild.
