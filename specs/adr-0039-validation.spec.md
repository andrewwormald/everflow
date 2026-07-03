---
goal: "Small polish: enrich `everflow version` with build info; add `everflow list` subcommand for enumerating Runs; add a startup-banner line from the daemon"
provider: github
project: andrewwormald/everflow
runner: claude
base_branch: main
base_repo: /Users/andreww/dev/everflow
concurrency: 1
draft_mrs: true
status: ready
---

# ADR-0039 validation: three tiny, independent CLI/observability items

The `goal:` field above is a **deliberate shopping-list format** — three items in one goal line. This spec's job is to exercise the planner-rationale-threading fix from [ADR-0039](decisions/0039-thread-planner-rationale-into-runner-goal.md). If the fix works, each of the three items lands as its own small MR. If the fix doesn't work, the runner will bundle them (recreating the mega-PR failure mode from Run `b21a0cc6`).

The items are trivially independent — different files, different concerns, no shared code path — so any bundling is unambiguously a scope-narrowing regression, not a legitimate design choice.

## Cross-cutting constraints (all MRs)

- **One concern per MR.** Each item below must ship in its OWN MR. Do NOT combine them. This is the load-bearing constraint that this spec exists to test.
- **British English.**
- **No customer-repo names.** Use `acme/example` in any new examples.
- **Every MR must include tests** proportional to the change. Docs-only MRs excepted.
- **Small diffs.** Each item is <100 LOC of change + a proportionate test. If your commit exceeds ~150 LOC total, you have almost certainly bundled items. Reconsider.

## Item A — Enrich `everflow version`

### What

The `version` subcommand today prints only `"0.0.1-scaffold"` (hard-coded in `main.go` as a package-level constant). Enrich it to print:

- Version string (as today)
- Git commit hash (short, 7 chars) — via `-ldflags "-X main.gitCommit=..."` set at build time, defaulting to `"unknown"` if unset
- Build timestamp — via `-ldflags "-X main.buildTime=..."` at build time, defaulting to `"unknown"`

Example output:
```
everflow 0.0.1-scaffold (commit: 1324ded, built: 2026-07-03T12:33:26+01:00)
```

### Scope

- `main.go` only. Two new package-level `var` (not `const`) fields so `-ldflags -X` can override at build time. Update `cmdVersion`. That's it.
- No Makefile, no build scripts (the ldflags are documented in a comment or a short section in `AGENTS.md`, but adding them to a Makefile would violate scope).
- Test: a small unit test on the version-formatting function (extract it if needed) that verifies the output shape given inputs.

### Done when

- `./everflow version` prints the enriched string.
- Test asserts the output format.
- `main.go` diff is < 40 LOC.

## Item B — `everflow list` subcommand

### What

Add `everflow list` — a read-only CLI that enumerates all Runs in the sqlite store, one line per Run, with brief status.

Example:
```
b21a0cc6-5c79...  spec  Completed  11 turns   goal: "Early-access hardening..."
83a6235c-...      spec  Failed     2 turns    goal: "Early-access hardening..."
cc2383f8-...      spec  Completed  11 turns   goal: "Refresh DESIGN.md..."
```

Sorted by created-at desc (newest first).

### Scope

- Add `list` subcommand to the `commands` map in `main.go`.
- Add a `cmdList` function that reads from the sqlite store, iterates records, prints the summary.
- Reuse the same `--store` flag semantics as `cmdStatus` (offline read).
- Test: a table-driven unit test that seeds a temp store with several records and asserts the output rows.

### Done when

- `./everflow list` prints all Runs in the store.
- Test covers empty store, single Run, and multi-Run cases.
- No `everflow status` behavioural change — this is purely additive.

## Item C — Startup-banner line in the daemon log

### What

When `everflow daemon` starts up, before the existing `"everflow daemon started"` log line, emit a single-line banner that summarises the process identity for humans grepping logs. Something like:

```
everflow 0.0.1-scaffold (commit: 1324ded) starting up — pid=12345, go=go1.26.0, os=darwin/arm64
```

Purely observability; no behavioural change.

### Scope

- `main.go`'s daemon startup path. One new `logger.Info` call, threading the same version fields item A introduced (so if you're doing item A too, this uses those fields; if item A hasn't shipped yet, use the existing `version` constant).
- Test: assert that the `daemon` subcommand's stderr output contains a banner line matching the expected shape. Can be done via a small integration test that spawns the daemon binary briefly then kills it.

### Done when

- Starting the daemon prints the banner before any other log line.
- Test asserts the banner line matches the expected shape.

## Ordering and dependency notes

- Items A and C share the version-info fields (item A introduces `gitCommit` and `buildTime`; item C uses them). The planner may want to pick A first so C can reference the fields. But if C ships first, it can use the existing `version` constant and be updated later — either order works.
- Item B is fully independent of A and C.
- Regardless of order, **the three items must ship in three separate MRs.**

## Done when (whole spec)

- Three merged PRs on `andrewwormald/everflow`, one per item.
- If any single PR touches all three items' concerns, ADR-0039's fix has regressed and this spec has served as the canary.
- Runner-token cost is materially lower than the b21a0cc6 comparable (which spent ~11 turns on 5 items bundled into 3 MRs) — expect ~6-8 turns for 3 items in 3 MRs.
