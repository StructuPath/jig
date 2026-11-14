# Gates and the test tripwire

What a gate checks, how a verdict must be stated, and what `tests_intact`
refuses. The field-level reference for gates — names, `budget`, `allow`,
failure messages — is the [Gates table](definition-reference.md#gates) in the
definition reference; this page is the behavior behind it. Source:
`internal/engine/gates.go`, `internal/engine/tests_intact.go`.

## Envelopes are the only inter-phase contract

Every agent phase ends with one JSON object carrying at least `status`
(`success` or `fail`). Extra fields — `approved`, `changed_files`, `revised`
— are what gates, `if:` guards, and repair predicates read. Code phases get
an adapter envelope built from their exit status, so a failing test suite
enters the repair loop through the same door as a failing agent report.

## A verdict is stated, never inferred

Under `verdict_consistent` a reviewer should write `approved` as a JSON
boolean. With no blocking item and no unmet finding, omitting it (or writing
a non-boolean) is its own gate failure, not a silent rejection. With a
blocking item or an unmet finding, the gate checks only that `approved` is
not `true`, so a missing verdict passes there as a rejection
(`internal/engine/gates.go`). Ask for the boolean in every reviewer prompt.
The distinction matters because `on_fail: {when: "approved == false"}` is how a
review routes work back to a builder, and a verdict nobody stated must not
decide that either way.

## Gates verify claims, they do not judge quality

The registry is `artifacts_exist`, `files_non_empty`, `diff_matches_claims`,
`verdict_consistent`, `tests_pass(command)`, and `tests_intact`.
Repo-specific verification is a code phase, not a new gate.

## tests_intact: a builder cannot cheaply buy green by weakening tests

`tests_intact` is a tripwire against careless or opportunistic test
tampering, not an adversarially complete analysis. Every test file changed
since `JIG_BASE_SHA` — committed, uncommitted, or untracked, gitignored
included — is compared whole against its base version: both sides are
normalized (comments stripped by a lexer that knows strings, regex literals,
and Ruby heredocs and %-literals; literals blanked, whitespace collapsed so
a token split across lines rejoins), then tests, assertions, and markers are
counted.

It fails when a test file is deleted, ends with fewer tests (Go `Example`
and `Fuzz` functions in a `_test.go` file included), assertions (a Go
example's `// Output:` or `// Unordered output:` line included, so deleting
expected output is a loss), or `m.Run(` calls, or with more of any marker
that stops tests running: a skip or focus (`t.Skip`, `test.only.each`, a
vitest options object with `skip`, `only`, `fails`, or `todo` set to
anything but `false`, `0`, `null`, `undefined`, or `""` — in any argument
position, after any first argument, through `.each(…)(…)`, with the key bare
or quoted, as in `test("…", {skip: true}, fn)` or `test(name(), fn, {"only":
1})`, `mark.skip` in any form, `pytest.importorskip(`, `xit`, RSpec `skip:
true` or `:focus`), a `//go:build` constraint, an added `TestMain` or
`os.Exit(`, `pytestmark`, `__test__ = False`, an aliased `mark`, or a
conftest `collect_ignore` or collection hook. Commenting a test out, or
wrapping it in `/* */` or a string, is a loss.

Counts are per file and per marker kind, so renaming a test inside its file,
converting `test(…)` to `test.each(…)`, swapping an assertion for a stronger
one, or rewording a skip's message passes, and new tests always do; renaming
a test FILE reads as a deletion.

What it cannot inspect fails closed: a binary or over-4 MiB test file, a
source test file with a NUL byte or a UTF-16/UTF-32 byte-order mark, a
comment, string, or heredoc still open at end of file, a tracked test file
flagged assume-unchanged or skip-worktree, an embedded git repository
holding test files. Untracked files under `node_modules`, Go `vendor`, and
Python virtualenvs are skipped, as the runners skip them. Each finding names
`path:line` where a changed line shows it, and says what to put back.

It covers Go, JavaScript and TypeScript, Python, and Ruby/RSpec in v1,
recognizing test files the way `factory.yaml`'s risk classifier does, plus
`conftest.py`.

When a change is meant to remove tests, say so in the definition: `{name:
tests_intact, allow: ["legacy/*_test.go"]}` exempts matching paths. `allow`
uses `path.Match` globs against the repo-relative path — `*` stops at `/`
and `**` is refused — and is rejected on any other gate. `factory.yaml` and
`factory-parallel.yaml` run it on `build`.

Known limits: it does not catch an assertion replaced by a trivially true
one, a test runner neutered through configuration outside `conftest.py`
(jest or pytest config, `package.json`, a `Makefile`), a skip hidden in a
non-test helper, a skip hidden inside a JS template-literal expression or
behind an exotic string escape, an existing marker whose arguments are
widened (`skipif(False)` → `skipif(True)`, edited `collect_ignore` contents,
`if testing.Short()` → `if true`), arithmetic that keeps `m.Run(` but
discards its result, `pytest.exit(..., 0)`, vitest options passed through a
variable (`const opts = {skip: true}`), spread (`{...opts}`) or nested in
another object, or written as `{ skip }` shorthand, a gutted `f.Fuzz(...)`
body, or changes inside submodules — the reviewers and the risk classifier
are the check there.

## tests_intact: nor by silencing the checkers around them

`tests_intact` also refuses a checker suppression or a "not implemented"
stub added to any changed source file, test or not, in the same four
languages — committed, uncommitted, or untracked and not gitignored.

A suppression is a comment that opens with a directive: Go `//nolint`,
`#nosec`, `//lint:ignore`; `eslint-disable` (any form), `oxlint-disable`,
`biome-ignore`, `deno-lint-ignore`, `prettier-ignore`, `@ts-ignore`,
`@ts-nocheck`, `@ts-expect-error`, `istanbul`/`c8`/`v8 ignore`; Python `#
noqa`, `# type: ignore`, `# pyright: ignore`, `# pyre-ignore`/`pyre-fixme`,
`# mypy: ignore-errors`, `# pylint: disable`, `# pragma: no cover`, `# ruff:
noqa`, `# nosec`, `# fmt: off`/`skip`; Ruby `# rubocop:disable`/`todo`, `#
standard:disable`, `# steep:ignore`, `# :nocov:`.

A stub is `panic`, `errors.New`, `fmt.Errorf`, `throw`, or `raise` whose
message starts "not implemented", "not yet implemented", "unimplemented", or
"TODO".

Counts are per directive, per file, and one removed anywhere in the change
offsets one added elsewhere, so keeping, narrowing, or removing an existing
suppression passes, and so does moving one — within its file, or to another
by a rename or a split, which `--no-renames` shows as a deletion and a new
file; prose that mentions a directive, a directive inside a string, and an
abstract method's bare `raise NotImplementedError` pass too.

An empty `catch {}` or `except: pass` is not refused: both have legitimate
uses (jig's own UI swallows a disabled-storage error), so the reviewers
judge them. A justified suppression is refused like any other; exempt its
path with `allow`.

Unlike a test file, a source file the gate cannot read — over 4 MiB, binary,
or left open at end of file by the lexer — is noted as uninspected, not
failed, because the builder may not be able to repair a base version the
lexer misreads.
