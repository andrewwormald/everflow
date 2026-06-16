# ADR-0015: Throttled-sequential MR flow with configurable concurrency

**Status**: Accepted
**Date**: 2026-06-16

## Context

Given the mandate in [ADR-0014](0014-refactor-sweep-mandate.md) — drive
bulk refactors MR-by-MR — there's a real question of *cadence*. Three
shapes were considered:

1. **All-at-once**: open all 47 MRs simultaneously. Fastest, but overwhelms
   reviewers and creates a merge-conflict bomb (each MR touches the codebase;
   if any merges, the rest need rebases).
2. **Pure reactive**: ride the normal stream of activity, processing units
   only when the universe brings them up. Sustainable, but no deadline and
   no guarantee of completion.
3. **Throttled-sequential**: open N MRs at a time; when one merges (or is
   closed), the next unit is picked up. Configurable cap.

## Decision

**Throttled-sequential flow with configurable concurrency, gated on
"prior MR merged."**

The Run holds a semaphore of N slots. A unit can enter `Working` (where it
opens an MR) only when a slot is free. A slot frees when the unit's MR
reaches a terminal state — `merged` releases the slot for the next unit;
`closed-not-merged` releases it and blacklists the unit.

```
   ┌──────────────────────────────────────────────┐
   │ semaphore: N slots                           │
   │                                              │
   │   Discovering → queue → Working (per unit)   │
   │                            │                 │
   │                            ▼                 │
   │                       Awaiting-merge ────────┼──► slot free
   │                            │                 │
   │                            │ merged          │
   │                            ▼                 │
   │                          Done (unit)         │
   └──────────────────────────────────────────────┘
```

Default `--concurrency 1` — strictly sequential. Bumping to 5 or 10 is a
flag at Trigger time.

## Alternatives considered

- **All-at-once (concurrency = unbounded)**: rejected. Overwhelms reviewers,
  creates merge conflicts. If the user has 47 services to update, dumping
  47 simultaneous MRs is the *exact thing this project exists to prevent*.
- **Pure reactive ("ride the stream")**: rejected as the *primary* shape.
  Can't promise completion by a deadline; misses services that don't get
  touched by other activity. Still valid as a *secondary* mode for cases
  where the user explicitly wants organic rollout (`--mode reactive`
  remains a future option).
- **Fixed concurrency (no user override)**: rejected. Different refactors
  need different cadences — a no-op rename can run at concurrency 10; a
  schema migration probably wants 1. Make it configurable.

## Consequences

- The Run holds the queue, the in-flight set, the completed set, and the
  blacklisted set in its durable `Object`.
- For v1, concurrency 1 ships first. The single-Run model is dramatically
  simpler to implement than the parent/child orchestration that
  concurrency > 1 requires (separate workflow Runs per in-flight unit,
  parent Run coordinating). Concurrency > 1 is an additive v2 increment.
- Slot release on `closed-not-merged` (reviewer rejected) means the
  refactor doesn't stall on a single bad unit — it blacklists and moves on.
  But the workflow pauses on "subagent gave up" or "CI permanently red,"
  which are *unit-internal* failures requiring author intervention. See
  [ADR-0017](0017-author-privilege-model.md) for the pause-resume flow.
- The "throttle on merged" event chain depends on webhooks. Without them
  we'd be polling for MR status, which defeats half the point. Webhook
  infrastructure is therefore load-bearing for this design (see DESIGN.md
  for the public-URL strategy).
- Completion criterion: discovery returns zero AND nothing is in-flight
  AND queue is empty → Run transitions to Completed. The daemon stays up
  for other Runs; only the Run itself terminates. See
  [ADR-0019](.) (TBD) for the daemon-vs-Run lifecycle distinction.
