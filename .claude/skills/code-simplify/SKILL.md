---
name: code-simplify
description: Simplify changed jig code for clarity over cleverness — remove duplication, dead code, needless abstraction, and confusing names without changing behaviour, proven by the existing tests. Use after a slice works and before review.
argument-hint: "[path | branch]"
---

# /code-simplify — clarity over cleverness

Make the code easier to read and change without changing what it does.
Behaviour is pinned by the tests: they pass before, they pass after, and no
test changes except to delete one that tested removed dead code.

## Steps

1. **Scope.** `$ARGUMENTS` names files or a branch; default is the files in
   `git diff main...HEAD`. Do not wander into unrelated code.
2. **Run the tests first** for the touched packages and record the result.
   If they are not green, stop: simplifying on red hides regressions.
3. **Look for**, in the scoped code:
   - duplication that a helper already in the repo covers — search before
     writing a new one;
   - dead code, unused parameters, unreachable branches;
   - abstraction with one caller and no planned second;
   - deep nesting that an early return flattens;
   - names that need a comment to explain them;
   - comments that restate the code instead of the *why*/plan ID.
4. **Apply one simplification at a time**, re-running the package tests after
   each. Revert any that changes behaviour or makes the code longer without
   making it clearer.
5. **Finish with the checks for what changed** — `gofmt`, `go vet ./...`,
   package tests; `npm run typecheck`, `npm test`, `npm run build` for UI.
6. **Report** each change in one line with the reason, and anything you
   left alone on purpose.

## Never

- Change public behaviour, API shape, definition schema, or migrations.
- Trade a clear five lines for a clever one.
