---
name: review
description: Review a branch or PR before merge for correctness, jig invariants, tests, and code health — report blocking vs optional findings with file:line evidence. Use before opening or merging a pull request.
argument-hint: "[PR number | branch]"
---

# /review — improve code health

Approve a change when it leaves the codebase healthier than it found it — not
when it is perfect. Every finding needs evidence; every blocking finding
needs a realistic path to the failure.

## Steps

1. **Get the diff.** `$ARGUMENTS` names a PR or branch; default is
   `git diff main...HEAD`. Read the linked plan record in `docs/plans/` if
   commits cite one, and judge the change against its requirements.
2. **Read the changed code in context**, not just the hunks: callers,
   tests, and the package's existing conventions.
3. **Check, in order:**
   1. **Correctness** — logic, error handling, nil/empty cases, state
      transitions, concurrency (claims, heartbeats, sweeps), resource
      cleanup.
   2. **jig invariants** —
      - worker never imports control plane (`just boundary`, KTD1);
      - definition changes validated in `internal/protocol/definition.go`
        with a test, stock definitions pass `just definitions`;
      - parallel-group members stay read-only (except `build_outputs`) and
        are never dispatched outside their group;
      - `web/src/` changes ship a rebuilt `web/dist/`;
      - UI imports are all in `web/package.json`;
      - migrations are additive and numbered after the latest
        (`migrations/NNN_<name>.sql`).
   3. **Security** — shell/command construction, path handling and the
      write boundary, secrets in logs, untrusted repo content.
   4. **Tests** — each changed behaviour has a test that would fail without
      it; no test was skipped or weakened.
   5. **Health** — naming, duplication, dead code, comments that explain why
      and cite plan IDs, `gofmt`.
4. **Verify each finding** before reporting it: trace the caller or input
   that triggers it. Drop what you cannot substantiate.
5. **Report** grouped as **Blocking** and **Optional (nit)**, each as
   `path:line — problem — consequence — suggested fix`. End with a one-line
   verdict: *approve*, *approve with nits*, or *changes requested*.

Do not edit code during a review unless asked; offer to apply fixes after.
