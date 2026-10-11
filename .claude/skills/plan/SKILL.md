---
name: plan
description: Plan how to build a spec'd change — add key technical decisions (KTD), small atomic implementation units (U), risks, and verification to the docs/plans/ record. Use after /spec and before /build.
argument-hint: "[path to plan record]"
---

# /plan — small, atomic tasks

Turn the requirements in a `/spec` record into decisions and units of work
that can each be built, tested, and committed on their own.

Use the record at `$ARGUMENTS`, or the newest file in `docs/plans/` if none is
given. If it has no `## Requirements` section, stop and run `/spec` first.

Do not write code in this step.

## Steps

1. **Read the code the units will touch.** Name real files, functions, and
   tests. A plan that cites `web/src/Tasks.tsx#storedProject` is checkable; one
   that says "the tasks page" is not.
2. **Add `## Key Technical Decisions`.** Number them `KTD1`, `KTD2`, … Each
   states the choice, the reason, and what was rejected:
   `*Rejected:* <alternative> — <why>`.
3. **Add `## Implementation Units`.** Number them `U1`, `U2`, … in build
   order. Each unit has:
   - **Goal** — one sentence; the requirements it satisfies (`R2, R4`).
   - **Files** — create / modify / test, by path.
   - **Approach** — enough that `/build` need not re-decide anything.
   - **Test scenarios** — the cases that prove it, including the edge cases.
   - **Verification** — the exact command(s), e.g.
     `go test ./internal/engine/ -run TestRepairEdge`, `npm test -- board`.

   A unit is the right size when it is one reviewable commit that leaves
   `just check` green. Split any unit that spans more than one layer
   (protocol, engine, controlplane, worker, runtime, web) unless the layers
   cannot change apart.
4. **Add `## Risks`** — what could go wrong and how the plan contains it.
5. **Add `## Verification (whole PR)`** — the full command list for the PR:
   `just check`, `just test-race` when engine/worker/controlplane/runtime
   change, and `npm run typecheck && npm test && npm run build` with a clean
   `git diff -- web/dist` when `web/src/` changes.
6. Check every requirement is covered by at least one unit. List any that
   are not, and either add a unit or move the requirement to *Deferred*.

## Done when

- Each unit can be handed to `/build` with no further design questions.
- Requirement → unit coverage is complete.
