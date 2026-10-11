---
name: test
description: Prove a change works — map each requirement to a test, fill gaps, and run the full jig check suite (just check, race suite, UI tests, bundle diff). Use before review or when asked whether something is actually tested.
argument-hint: "[path to plan record | package | file]"
---

# /test — tests are proof

A requirement is met when a test that would fail without the change passes
with it. Everything else is a claim.

## Steps

1. **Find the scope.** The plan record in `$ARGUMENTS` (or the newest in
   `docs/plans/`), else the branch diff: `git diff main...HEAD --stat`.
2. **Build a requirement → test map.** For every `R<n>` (or, without a plan,
   every behaviour the diff changes), name the test that proves it:
   `R3 → internal/engine/engine_test.go: TestRepairEdgeBudgetExhausted`.
   Mark each row *covered*, *weak* (asserts too little, or would pass
   without the change), or *missing*.
3. **Close the gaps.** Write the missing and weak tests. Cover edge cases the
   plan named plus the usual suspects for jig: empty and unknown states,
   budget exhaustion, cancelled/timed-out attempts, concurrent claims, a
   definition that fails save-time validation.
4. **Check the tests can fail.** For each new test, briefly break the code it
   covers (or reason precisely about why it would fail) — then restore it.
5. **Run the suite that CI runs:**
   - `just check` — format, vet, boundary, definitions, tests, build.
   - `just test-race` if `internal/{controlplane,worker,engine,runtime}`
     changed.
   - From `web/`: `npm run typecheck`, `npm test`, `npm run build`, then
     `git diff --exit-code -- web/dist` from the root.
6. **Report** the map, the commands with pass/fail, and any failure output
   verbatim. Never call a failing or skipped check "passing".

## Never

- Skip, disable, or loosen a test to get green.
- Count a test that cannot fail as coverage.
