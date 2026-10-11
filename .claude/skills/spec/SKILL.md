---
name: spec
description: Define what to build before any code — write the problem frame, numbered requirements, and scope boundaries as a new plan record in docs/plans/. Use when starting a feature, fix, or refactor in jig.
argument-hint: "<what to build>"
---

# /spec — spec before code

Write down *what* and *why* before anyone decides *how*. The output is the
first half of a plan record in `docs/plans/`; `/plan` fills in the second half.

Do not write or edit code in this step.

## Steps

1. **Read before asking.** Read `README.md` (definition → run → job → attempt
   → phases), `AGENTS.md`, and the plan records in `docs/plans/` that touch the
   same area. Search the code for the behaviour in question so the spec
   describes the system as it is, not as remembered.
2. **Ask what you cannot find.** If the goal, the user, or "done" is unclear
   from `$ARGUMENTS` and the code, ask before writing. One round of focused
   questions beats a spec built on guesses.
3. **Create the record** at
   `docs/plans/<YYYY-MM-DD>-<NNN>-<type>-<slug>-plan.md`, where `<NNN>` is the
   next free number for that date and `<type>` is `feat`, `fix`, or
   `refactor`. Match the existing records' shape:

   ```markdown
   ---
   title: "<type>: <one-line title>"
   type: <type>
   date: <YYYY-MM-DD>
   ---

   # <type>: <one-line title>

   **Target repo:** `StructuPath/jig`. <Which layers it touches: Go, API,
   migration, UI — and which it does not.>

   ---

   ## Summary
   ## Problem Frame
   ## Requirements
   ## Scope Boundaries
   ### Deferred to Follow-Up Work
   ```

4. **Requirements are numbered `R1`, `R2`, …** Each one is a single
   observable behaviour that a test or a manual check can prove. Write
   "the board shows exactly five columns", not "the board is clear".
   Commits and code comments will cite these IDs.
5. **Scope Boundaries** names what this work will *not* do, and why. Anything
   tempting but out of scope goes under *Deferred to Follow-Up Work*.
6. Call out which jig invariants the change is near: the worker/control-plane
   boundary (KTD1 in the factory plan), save-time definition validation,
   parallel-group members being read-only, the committed `web/dist/` bundle.

## Done when

- Every requirement is testable and numbered.
- An engineer who has not seen this conversation could say what "done" means.
- No implementation choices have crept in — those belong in `/plan`.
