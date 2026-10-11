---
name: constraints
description: Set a quality bar once and enforce it everywhere — record a new project rule in AGENTS.md and back it with an automated check (just recipe, test, or CI step). Use when a mistake should never recur or a convention needs to become a rule.
argument-hint: "<the rule to adopt>"
---

# /constraints — decide it once, enforce it everywhere

A rule that only lives in someone's head, or only in prose, will be broken.
Every constraint gets a written rule *and* a check that fails when it is
broken.

## Steps

1. **State the rule** from `$ARGUMENTS` in one sentence, then ask what it
   protects against. If the reason is unclear, ask before going further.
2. **Check it is not already a rule.** Read the `## Rules` list in
   `AGENTS.md`, the `Justfile`, `.github/workflows/`, and the plan records.
   If it exists, tighten the existing one instead of adding a duplicate.
3. **Find the current violations.** Search the tree. Report the count and
   locations. Fix them in this change if few; otherwise propose a separate
   cleanup and make the check report them without blocking until then.
4. **Choose the cheapest enforcement that cannot be skipped**, in order of
   preference:
   1. A compile- or save-time error (Go types, `internal/protocol`
      validation, TypeScript).
   2. A test in the package that owns the behaviour.
   3. A `just` recipe wired into `just check` (as `boundary` and
      `definitions` are), so CI runs it on Linux and macOS.
   4. A CI step in `.github/workflows/` (as the `web/dist` diff is).

   Prose-only is the last resort and must say why nothing automated fits.
5. **Write the rule into `AGENTS.md` → `## Rules`** as a bold one-line
   statement plus the reason, matching the existing entries, and name the
   check that enforces it.
6. **Prove the check works:** introduce a violation, see the check fail,
   remove it, see it pass. Then run `just check`.

## Done when

- `AGENTS.md` states the rule and names its check.
- The check fails on a violation and passes on the clean tree.
