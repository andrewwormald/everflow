# ADR-0042: Unrecognised `/everflow` verbs become freeform instructions

**Status**: Accepted
**Date**: 2026-07-16

## Context

[ADR-0017](0017-author-privilege-model.md) defines a fixed control-verb set
(`pause`, `resume`, `skip`, `retry`, `prompt`, `status`, `stop`, `abandon`)
plus a fallback: any other verb got "Unknown command `/everflow foobar`.
Reply `/everflow` for the verb list." (`cmdUnknown` in `controls.go`).

That fallback is a dead end for the author. If they type
`/everflow refactor the auth module first` — a perfectly reasonable
instruction, just not one of the eight fixed verbs — everflow bounces it
instead of acting on it. The author's only recourse is to remember the
exact `/everflow prompt <text>` syntax and retype the same words after it.
`/everflow prompt` itself only *stores* the instruction for the *next*
runner invocation (`work()`, `discoverSpec()`, or a future `invokeForEvent`
call) — it doesn't act immediately, so even using the right verb doesn't
get the author an immediate response.

## Decision

`handleControlCommand`'s `default` case no longer replies "Unknown
command." It now treats the whole text after `/everflow ` (verb + args,
recomputed from the raw comment body to preserve casing and multi-line
formatting) as a freeform instruction and acts on it immediately:

1. Look up the in-flight unit for the event's MR (`unitForMR`, the same
   helper `resume()` uses for ordinary events). If the MR isn't tracked,
   reply with a "not tracked" message (mirroring `cmdSkip`'s guard) — there's
   no subagent to direct.
2. Otherwise, stash the instruction in `AgentState.PromptInjection` — the
   same single-use slot `/everflow prompt` writes to — and call
   `invokeForEvent(ctx, r, unitID, ev)` directly, reusing the exact code
   path a `NoteAdded` event takes when the Starlark filter decides
   `INVOKE_SUBAGENT`. `invokeForEvent` prepends `PromptInjection` to
   `req.Goal` and consumes it (clears it) as it always does, then runs the
   configured runner, commits/pushes on `DecisionDone`, and posts the usual
   acknowledgement comment.

This means a freeform verb is really `/everflow prompt <text>` fused with
an immediate trigger, rather than a ninth hand-written verb — no new
runner-invocation logic, no new state field.

Because control commands are detected and dispatched *before* the
`StatusPaused` early-return in `resume()` (existing behaviour, since
`pause`/`resume`/`stop`/`retry` all need to work while paused), a freeform
instruction also fires while paused, unparking the Run in the same way
`/everflow retry` does. This is intentional: like the other control verbs,
issuing one is an explicit author action, not an event the filter decided
to act on.

`helpMessage` gained a line documenting the fallback so `/everflow` (bare)
tells the author this now works, instead of implying only the eight fixed
verbs are valid.

## Alternatives considered

- **Keep the "Unknown command" bounce, just improve the wording.** Doesn't
  solve the underlying problem — the author still has to know and retype
  `/everflow prompt`.
- **Auto-detect "looks like a verb" vs "looks like a sentence" and only
  freeform the latter.** Adds a heuristic with its own false-positive/
  negative surface (is `/everflow squash` a verb we forgot or an
  instruction to squash commits?) for no real benefit — routing everything
  unrecognised through the same path is simpler and the worst case is
  identical either way (the subagent gets an instruction it may not be able
  to act on, same as any freeform comment already routed via the filter).
- **New dedicated state field for the freeform instruction instead of
  reusing `PromptInjection`.** Rejected per the increment's scope — reusing
  the existing single-use slot means `invokeForEvent`, `work()`, and
  `discoverSpec()` don't need to learn about a second injection source, and
  `buildStatusComment`'s existing "Pending prompt injection: yes" line
  stays accurate without changes.

## Consequences

- Any `/everflow <anything>` on a tracked MR now triggers an immediate
  subagent invocation with that text as the goal, not just the eight fixed
  verbs. Typos in verb names (e.g. `/everflow satus`) now invoke the
  subagent with "satus" as (part of) its instruction instead of surfacing a
  clear "did you mean" error — a minor regression in typo feedback, traded
  for the freeform capability.
- `cmdUnknown` is removed; `cmdFreeform` takes its place in the `default`
  case of `handleControlCommand`.
- Freeform verbs on an untracked MR still get a clear reply (mirroring
  `cmdSkip`), so the "not tracked" case isn't silently swallowed.
- Consistent with `/everflow retry`, a freeform verb unparks a paused Run —
  authors relying on `/everflow pause` for a hard stop should use
  `/everflow stop` or `/everflow abandon` instead, which remain terminal.
