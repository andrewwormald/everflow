# Everflow — design

**Status**: Active design. The v0 scheduled-skill code in this repo demonstrates the durable-workflow plumbing but does not yet implement the v1 mandate described below. Every load-bearing decision in this doc links to an ADR under [`decisions/`](decisions/).

**Last updated**: 2026-06-16

## Mandate

Everflow drives **bulk refactor sweeps over large codebases**. It opens MRs one (or N) at a time, watches them through review and CI, addresses reviewer feedback, ships the merge, and picks up the next unit — until the refactor is done or the user calls a halt.

The point isn't durability for its own sake. The point is **amortising LLM cost**: the workflow handles the repetitive cheap work (queueing, throttling, status reporting, classifying comments, retrying flakes) deterministically; the subagent fires only on the bits that need reasoning. See [ADR-0014](decisions/0014-refactor-sweep-mandate.md).

## Why a workflow library for this

Bulk refactors have an L1/L2/L3 shape that maps cleanly onto `luno/workflow`:

| Refactor concept | `luno/workflow` primitive | ADR |
|---|---|---|
| The refactor goal (one-time setup) | `Trigger(WithInitialValue(&AgentState{Goal, Units, Filter, Skill}))` | [0005](decisions/0005-context-in-workflow-run.md) |
| Throttled sequence (per-unit state machine) | Step graph with a `Working → Awaiting-merge → ...` cycle, semaphore in `AgentState` | [0015](decisions/0015-throttled-sequential-mr-flow.md) |
| Wait for MR events (no polling, no busy loop) | `AddCallback` fired by inbound webhooks | [0014](decisions/0014-refactor-sweep-mandate.md) |
| Resume after restart | `RecordStore`; `Run.Object` rehydrated identically | [0005](decisions/0005-context-in-workflow-run.md) |
| Author intervention ("I'm stuck") | `Pause()` + author posts `/everflow resume` comment | [0017](decisions/0017-author-privilege-model.md) |
| Concurrency > 1 (v2) | Parent Run + child Runs per in-flight unit, queue + semaphore in parent | [0015](decisions/0015-throttled-sequential-mr-flow.md) |
| Audit trail | Event log + the MR thread itself | [0016](decisions/0016-mr-comments-only-channel.md) |

## Architecture

```
                ┌──────────────────────────────────────────┐
   Author ───►  │   everflow CLI                           │
   (Claude or   │   start / status / phrases promote / ... │
    direct)     └──────────────────┬───────────────────────┘
                                   │ trigger / inspect
                                   ▼
                ┌──────────────────────────────────────────┐
                │   everflow daemon (long-lived)           │
                │   ┌─────────────────────────────────┐    │
                │   │ luno/workflow runtime           │    │
                │   │  - RecordStore (sqlite)         │    │
                │   │  - TimeoutStore (sqlite)        │    │
                │   │  - EventStreamer (in-process)   │    │
                │   └────────────────┬────────────────┘    │
                │                    │                     │
                │   ┌────────────────┴────────────────┐    │
                │   │ Per-Run state machine           │    │
                │   │  - Starlark filter eval         │    │
                │   │  - Provider client (glab/gh)    │    │
                │   │  - Runner (claude -p, ...)      │    │
                │   │  - Worktree mgmt                │    │
                │   └─────────────────────────────────┘    │
                │                                          │
                │   ┌─────────────────────────────────┐    │
                │   │ HTTP server :8080               │    │
                │   │  POST /webhook/{provider}/{runID}│   │
                │   │  HMAC-verify → workflow.Callback│    │
                │   └────────────────▲────────────────┘    │
                └────────────────────┼─────────────────────┘
                                     │ webhook POST
                                     │ (via ngrok / tailscale funnel
                                     │  / public DNS on cloud)
                                     │
                ┌────────────────────┴─────────────────────┐
                │  GitLab / GitHub                         │
                │   - MR events: notes, pipelines,         │
                │     merge_request actions                │
                │   - Project-scoped webhook (one per      │
                │     project, dispatched to many Runs)    │
                └──────────────────────────────────────────┘
```

### Daemon model

- **Single long-lived process** (see [ADR-0003](decisions/0003-single-long-lived-daemon.md))
- Hosts the workflow runtime, sqlite store, embedded HTTP server, and per-Run state machines
- Runs under `launchd` / `systemd` / `tmux` — designed to live on a VPS or EC2 instance, not a laptop
- Single workflow definition (`refactor-sweep`); multiple Runs per workflow

### Public-URL strategy

Webhooks need a public address. v1 takes a `--public-base-url https://...` flag at daemon start; the user provides it via whatever tunneling fits their environment:

| Deployment | Tunnel |
|---|---|
| VPS / EC2 / DigitalOcean | Native public DNS; open the port in the security group |
| Laptop (preferred) | Tailscale Funnel — free, static `*.ts.net` URL |
| Laptop (alternative) | Cloudflare Tunnel — free with own domain |
| Laptop (last resort) | ngrok paid for stable URL; ngrok free works with the caveat that URLs rotate |

Everflow does not auto-spawn any tunnel. Too much magic, four tools to maintain, all of them flaky in their own ways. Users pick one and configure it.

## The state machine

Two concentric loops: a Run-level loop that handles discovery and queue management; a per-unit lifecycle that opens and ships one MR.

### Run-level loop (concurrency = 1, v1 baseline)

```
Initiated
   │ setup: register webhook with provider, build skill mirror, run discovery
   ▼
Discovering ───────── discovery returned 0 ────► Completed (post final comment, exit Run)
   │
   │ found N units, queue them
   ▼
Working                                        ◄─────────────────────────┐
   │ pop next unit from queue                                              │
   │ check filter, run subagent, push branch, open MR                      │
   ▼                                                                      │
Awaiting-merge ── [see per-unit lifecycle below] ─►                        │
                                              merged ────► slot freed ────┤
                                              closed ────► slot freed,    │
                                                           blacklist ─────┘
```

### Per-unit lifecycle (inside Awaiting-merge)

```
Awaiting-merge (sitting indefinitely, zero compute cost)
   │
   ├── webhook: note_added ──► filter ──► SKIP            (no cost)
   │                                  ──► CONTROL_COMMAND (author only — execute verb)
   │                                  ──► INVOKE_SUBAGENT (subagent reads thread, pushes fix)
   │                                                       │
   │                                                       └─► back to Awaiting-merge
   │
   ├── webhook: pipeline_failed ──► classify (Starlark)
   │                                  ──► known flake ────► retry job, no subagent
   │                                  ──► novel failure ──► subagent diagnose + fix
   │                                                         │
   │                                                         └─► back to Awaiting-merge
   │                                  ──► subagent stuck (3 attempts, same failure)
   │                                                         │
   │                                                         └─► Paused, post pause comment
   │
   ├── webhook: merge_request action=merged ──► Done (unit), release slot, return to Discovering
   │
   └── webhook: merge_request action=closed ──► Failed (unit), release slot, blacklist
```

The `Awaiting-merge` state can hold for *hours or days* with no compute load — the workflow is genuinely idle, waiting for the platform to push it an event. This is what makes the math work for long-running refactors.

### Concurrency > 1

Out of scope for v1, but the design accommodates it. The single Run becomes a *parent* Run holding a queue + semaphore; each in-flight unit gets its own *child* Run with the per-unit lifecycle. The parent's only job is queue management; child Runs handle their own MR. See [ADR-0015](decisions/0015-throttled-sequential-mr-flow.md) for the rationale.

## Communication model

The MR thread is the only channel ([ADR-0016](decisions/0016-mr-comments-only-channel.md)). Everflow speaks by posting comments; the human speaks by replying.

### Comment classification on inbound

Every `note_added` event runs through:

1. **Author + `/everflow ...` prefix?** → control command, see [ADR-0017](decisions/0017-author-privilege-model.md)
2. **Bot?** → provider-specific deterministic handler (e.g. Danger title-check → auto-fix title via `glab mr update --title`)
3. **Otherwise** → Starlark filter, see [ADR-0018](decisions/0018-starlark-filter-and-phrase-learning.md). Cheap-skip path for emojis and known phrases; subagent invocation only on substantive content.

### Author privileges

Captured at Trigger via `glab api user` (or `gh api user`), stored on `AgentState.Author`. From there:

- Author's `/everflow <verb>` comments → bypass the LLM, route to state transitions
- Reviewer comments → go through the filter (substantive ones may invoke the subagent to address them)
- Bot comments → per-source handling

Verbs: `pause`, `resume`, `skip`, `retry`, `prompt <text>`, `status`, `stop`. Full semantics in [ADR-0017](decisions/0017-author-privilege-model.md).

## Workflow inputs at Trigger time

The author hands everflow five things at `everflow start`:

| Input | What it is | Example |
|---|---|---|
| **Goal** | One-sentence human description | "Migrate all Go services from logrus to log/slog" |
| **Discovery rule** | How to find units. Either a `--units` static list, or a Starlark `discover()` function, or a shell command | `discover.star` walks `services/*/go.mod` for logrus imports |
| **Skill** | A Claude Code skill the per-unit subagent will run. Lives at `~/.everflow/runs/<runID>/SKILL.md` (mirror-symlinked into the worktree's `.claude/skills/`) | A skill file with the refactor recipe |
| **Filter** | Starlark function. Runs on every event. Defaults to a sensible one if not specified | `note_added.star`, `pipeline_failed.star` |
| **Provider config** | Which platform, project ID, auth token | `--provider gitlab --project acme/example` |

Plus a few operational flags: `--concurrency 1`, `--public-base-url https://...`, `--max-tokens 1M`, `--max-units 50`.

## Per-Run filesystem layout

```
~/.everflow/
├── runs/
│   └── <runID>/
│       ├── SKILL.md              # canonical skill, edited by subagent each iteration
│       ├── discover.star         # Starlark discovery rule (if supplied)
│       ├── note_added.star       # Starlark filter for review comments
│       ├── pipeline_failed.star  # Starlark filter for CI events
│       ├── phrases.yaml          # learned skip phrases (per-Run scope)
│       └── worktree/             # the git worktree, mirror-symlinks SKILL.md from above
├── phrases.global.yaml           # cross-Run defaults; human-curated only
├── store.db                      # sqlite RecordStore + TimeoutStore
└── daemon.pid
```

## The Runner interface (unchanged from prior design)

```go
type Runner interface {
    Name() string
    Run(ctx context.Context, req RunRequest) (RunResponse, error)
}

type RunRequest struct {
    Worktree     string
    SkillCommand string         // "/refactor-logrus-to-slog services/payments"
    Goal         string
    UnitContext  string         // bounded — only this unit's scope
    Budget       Budget
}

type RunResponse struct {
    Decision   Decision         // continue | ask | done | fail
    Summary    string
    Question   string           // populated when Decision == ask
    Learnings  Learnings        // { add_phrases: [...], skill_updates: "..." }
    Tokens     int
}
```

Backwards-compatible with the v0 scheduled-skill code; the refactor flow just uses richer fields (Learnings, UnitContext).

## Provider abstraction

```go
type Provider interface {
    Name() string
    AuthenticatedUser(ctx context.Context) (User, error)
    RegisterWebhook(ctx context.Context, projectID string, url string, secret string) (webhookID string, err error)
    DeregisterWebhook(ctx context.Context, projectID string, webhookID string) error
    VerifySignature(headers http.Header, body []byte, secret string) bool
    NormaliseEvent(headers http.Header, body []byte) (Event, error)

    CreateMR(ctx context.Context, branch, title, description string) (MR, error)
    PostComment(ctx context.Context, mr MR, body string) error
    UpdateMRTitle(ctx context.Context, mr MR, title string) error
    RetryPipelineJob(ctx context.Context, jobID string) error
    CloseMR(ctx context.Context, mr MR) error
    IsBot(user User) bool
}
```

v1 ships `gitlab.Provider`. v2 adds `github.Provider`. Implementations are ~150 LOC each.

## What's not yet built

The v0 code in this repo (`agent.go`, `main.go`, etc.) implements the scheduled-skill loop from [ADR-0010](decisions/0010-scheduled-skill-poc-first.md). It's retained as reference for the workflow primitives but does *not* implement this design.

The v1 implementation needs (in roughly this build order):

1. **Provider abstraction + GitLab adapter** — webhook register/dispatch, MR create/comment, signature verify
2. **HTTP server in the daemon** — `:port/webhook/{provider}/{runID}` route, HMAC verify, workflow.Callback dispatch
3. **Sqlite store** — replace memrecordstore/memtimeoutstore so daemon restart preserves Runs
4. **State machine for the refactor-sweep workflow** — `Discovering → Working → Awaiting-merge → ...`
5. **Starlark filter integration** — `go.starlark.net` embedded, filter eval per event, phrase file read/append
6. **Per-Run filesystem layout + skill mirror** — `~/.everflow/runs/<runID>/...`
7. **Control command handler** — author-only comment commands
8. **`everflow start` CLI** — flag parsing, validation, trigger; `everflow status`, `everflow phrases promote`
9. **GitHub provider adapter** — second implementation to validate the Provider interface

Rough sizing: ~2 weeks of focused work to ship the v1 baseline (concurrency = 1, GitLab only).

## Open questions

These don't block the architecture, but they shape implementation:

1. **Signal that learning is working** — the filter should call the LLM less often over time. What's the measurable signal? A counter of `subagent_invocations / total_events` per Run? Plotted where? Worth thinking through before we ship "the system gets smarter."

2. **Subagent invocation atomicity** — when processing a unit, does one `claude -p` invocation (a) read the skill, (b) make the change, (c) open the MR, (d) update the skill with learnings, in one call? Or are those separate calls? Per-unit-one-call is cheaper; multi-call is more verifiable. Default to one call for v1; reconsider if quality is poor.

3. **What about questions in reviewer comments?** A reviewer asking "why this approach?" isn't a blocking change request. Does the subagent answer it (post a reply comment) or wait for the human? Leaning *answer* — but this lets the subagent author content, not just code. Worth a small UX experiment.

4. **Dependencies between units** — some refactors have ordering constraints (unit B depends on unit A being merged first). Do we model this? v1 says no — the discovery rule is responsible for enumerating units that *can* run in any order. If ordering matters, the author writes a discovery rule that returns the next unit when the prior is shipped, not all units up front.

5. **Re-discovery cadence and cost** — does discovery re-run after every merge, or on a timer, or both? Cheap for grep-the-filesystem rules; expensive for "call an API to enumerate." Probably configurable.

6. **Skill mutation safety** — the subagent edits `SKILL.md` after each unit. What if it edits it badly and the next unit's subagent now does worse work? Version it (every change is a git commit on the mirror), and add `everflow skill-history <runID>` so the author can roll back.
