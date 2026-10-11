---
name: ship
description: Ship a finished branch — run the full CI-equivalent checks, confirm the committed web bundle, push the branch, and open a pull request. Never pushes to main. Use when the work is built, tested, and reviewed.
argument-hint: "[PR title]"
disable-model-invocation: true
---

# /ship — faster is safer

Small, frequent, verified pull requests are safer than large, rare ones.
Shipping in jig means a green PR from a branch — never a push to `main`.

## Steps

1. **Refuse `main`.** If the current branch is `main`, stop and ask for a
   branch name.
2. **Check the tree.** `git status` is clean apart from intended changes;
   nothing unrelated is staged.
3. **Run what CI runs**, and stop on the first failure with its output:
   - `just check`
   - `just test-race` if `internal/{controlplane,worker,engine,runtime}`
     changed
   - if `web/` changed, from `web/`: `npm ci` (when the lockfile changed),
     `npm run typecheck`, `npm test`, `npm run build`, then from the root
     `git diff --exit-code -- web/dist`
4. **Size check.** If the diff is large (`git diff main...HEAD --stat`)
   or spans unrelated concerns, propose splitting before shipping.
5. **Push** with `git push -u origin <branch>`.
6. **Open the PR** against `main`. Title from `$ARGUMENTS` or the lead
   commit, conventional style. Body:
   - **Summary** — what and why, linking the `docs/plans/` record and the
     R/U IDs it delivers;
   - **Verification** — the commands from step 3 and their results;
   - **Risks / follow-ups** — from the plan's Risks and Deferred sections.
7. **Watch CI** on the PR. A red check is this PR's work until proven
   otherwise: fix and push, never skip a test or re-run blindly.

## Never

- Push to `main`, force-push a shared branch, or merge your own PR without
  review.
- Ship with a check skipped or a test disabled.
