# Architecture Decision Records

A log of every meaningful decision made in this repo. The goal is
**agentifiability**: an AI agent should be able to read this directory and
have the same context a senior contributor would.

## Format

Each ADR is a numbered markdown file:

```
NNNN-kebab-case-title.md
```

Number is 4-digit zero-padded, assigned by the next unused integer. Titles
should be a short noun phrase ("Single long-lived daemon," not "We chose…").

### Template

```markdown
# ADR-NNNN: <title>

**Status**: Accepted | Proposed | Superseded by ADR-XXXX
**Date**: YYYY-MM-DD

## Context

What situation prompted this? What constraints were we operating under?
Reference prior ADRs by number and title.

## Decision

What we chose, stated as a single positive sentence first, then detail.

## Alternatives considered

Other options we looked at and why we did not pick them. Be specific —
"X is more complex" is weak; "X requires a long-lived subprocess we'd have
to manage across daemon restarts" is useful.

## Consequences

What this commits us to. What it rules out. What follow-up decisions it
forces or unlocks.
```

## Index

| #    | Title                                              | Status   | Date       |
|------|----------------------------------------------------|----------|------------|
| 0001 | Everflow is an orchestrator, not a Claude replacement | Accepted | 2026-06-15 |
| 0002 | Distribute as a Claude Code Skill + Go binary      | Accepted | 2026-06-15 |
| 0003 | Single long-lived daemon, not cron-driven ticks    | Accepted | 2026-06-15 |
| 0004 | Shell out to `claude -p`, not the Anthropic SDK    | Accepted | 2026-06-15 |
| 0005 | Context lives in the workflow Run, not in the runner | Accepted | 2026-06-15 |
| 0006 | One git worktree per Run                           | Accepted | 2026-06-15 |
| 0007 | Pluggable `Runner` interface, per-Run `--runner` flag | Accepted | 2026-06-15 |
| 0008 | Native structured output per runner                | Accepted | 2026-06-15 |
| 0009 | Collapse the step graph to `Initiated → Iterating → terminals` | Accepted | 2026-06-15 |
| 0010 | Build the scheduled-skill PoC before the interactive loop | Accepted | 2026-06-15 |
| 0011 | Persistent worktree, reset --hard to base each pass | Accepted | 2026-06-15 |
| 0012 | Track all decisions as ADRs                        | Accepted | 2026-06-15 |
| 0013 | Adopt the "Orbit" brand mark and warm-coral palette | Accepted | 2026-06-15 |
| 0014 | Bulk-refactor sweeps are the primary use case      | Accepted | 2026-06-16 |
| 0015 | Throttled-sequential MR flow, configurable concurrency | Accepted | 2026-06-16 |
| 0016 | MR comments as everflow's only communication channel | Accepted | 2026-06-16 |
| 0017 | Author-vs-reviewer privilege model + control commands | Accepted | 2026-06-16 |
| 0018 | Starlark filter on every event, with bounded phrase learning | Accepted | 2026-06-16 |
| 0019 | Project layout: `main.go` at root, business logic under `internal/`, `_v0/` archived as own module | Accepted | 2026-06-17 |
| 0020 | GitLab provider — hand-rolled REST client, bare-token webhooks  | Accepted | 2026-06-18 |
| 0021 | GitHub provider — HMAC webhooks, three comment events collapse to one | Accepted | 2026-06-19 |
