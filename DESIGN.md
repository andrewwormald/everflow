# Everflow — design

**Status**: Active design. The v1 baseline is implemented under `internal/` (refactor-sweep workflow, sqlite store, GitLab + GitHub providers, poller, webhook server). Every load-bearing decision in this doc links to an ADR under [`decisions/`](decisions/).

**Last updated**: 2026-06-29

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
                │   │  - EventStreamer (cond.Wait;    │    │
                │   │    internal/eventstream — see   │    │
                │   │    ADR-0033)                    │    │
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
                │   │ Poller (default; ADR-0031)      │    │
                │   │  30s tick → GetMRState +        │    │
                │   │  ListNotesSince → synth Event   │    │
                │   │  → workflow.Callback            │    │
                │   └─────────────────────────────────┘    │
                │                                          │
                │   ┌─────────────────────────────────┐    │
                │   │ HTTP server :8080 (opt-in)      │    │
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

### Event ingress: poll by default, webhook opt-in

Per [ADR-0031](decisions/0031-polling-as-primary.md), the default event source is **polling**: the daemon ticks every 30s and asks the provider what's new for each in-flight MR via `GetMRState` + `ListNotesSince`. Synthesised events flow through the same `workflow.Callback` path webhooks use. No public URL required; no webhook permissions needed; no tunnel; no `--public-base-url`. The "Never polls" brand promise was always about **LLM tokens** — polling a provider's REST API costs zero tokens.

Webhook mode remains supported for VPS / cloud deployments that have a stable URL and want sub-second latency. Set `event_source: webhook` in the spec and pass `--public-base-url https://...`. The user provides the tunnel via whatever fits their environment:

| Deployment | Tunnel |
|---|---|
| VPS / EC2 / DigitalOcean | Native public DNS; open the port in the security group |
| Laptop | Tailscale Funnel — free, static `*.ts.net` URL |
| Laptop (alternative) | Cloudflare Tunnel — free with own domain |
| Laptop (last resort) | ngrok paid for stable URL; ngrok free works with the caveat that URLs rotate |

Everflow does not auto-spawn any tunnel.

## The state machine

Two concentric loops: a Run-level loop that handles discovery and queue management; a per-unit lifecycle that opens and ships one MR.

### Run-level loop (concurrency = 1, v1 baseline)

```
Initiated
   │ setup: capture author, build skill mirror, run discovery
   │        (poll mode: skip webhook step; webhook mode: register + store secret)
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
   ├── event: note_added ──► filter ──► SKIP            (no cost)
   │                                  ──► CONTROL_COMMAND (author only — execute verb)
   │                                  ──► INVOKE_SUBAGENT (subagent reads thread, pushes fix)
   │                                                       │
   │                                                       └─► back to Awaiting-merge
   │
   ├── event: pipeline_failed ──► classify (Starlark)
   │                                  ──► known flake ────► retry job, no subagent
   │                                  ──► novel failure ──► subagent diagnose + fix
   │                                                         │
   │                                                         └─► back to Awaiting-merge
   │                                  ──► subagent stuck (3 attempts, same failure)
   │                                                         │
   │                                                         └─► Paused, post pause comment
   │
   ├── event: merge_request action=merged ──► Done (unit), release slot, return to Discovering
   │
   └── event: merge_request action=closed ──► Failed (unit), release slot, blacklist
```

The `Awaiting-merge` state can hold for *hours or days* with no LLM cost — between events the workflow consumes only the cost of the 30s poll tick (a couple of cheap REST calls), or sub-second wakeups on a webhook delivery. This is what makes the math work for long-running refactors.

Per [ADR-0034](decisions/0034-comment-loop-and-paused-self-loop.md): when the runner addresses a `note_added` event and successfully pushes the change, the originating discussion thread is auto-resolved on the platform — the reviewer sees their comment marked closed without manual action. If the runner replied without producing a code change (e.g. answered a question verbally), the thread is also resolved and the Run stays `Awaiting-merge` (no fatal pause for non-code answers). `Paused → Paused` is a permitted self-loop in the callback graph so events that arrive during a pause (including `/everflow resume`) flow through `resume()` instead of being dropped as illegal transitions.

Per [ADR-0032](decisions/0032-staging-filters-binary-blobs.md): the worktree commit step stages tracked modifications via `git add -u`, then iterates `git ls-files --others --exclude-standard` and skips any file whose first 512 bytes contain a NUL byte. Build artefacts produced by runner self-verification (`go build`, `go test`) therefore don't land in the MR. If filtering leaves nothing staged the unit is blacklisted (same semantics as "runner reported Done with no changes"); the Run continues. Step bodies return `nil` on terminal failure so luno/workflow doesn't retry permanent errors forever.

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

A Run is triggered from a **spec file** — markdown with YAML frontmatter (ADR-0024). The frontmatter carries the structured config; the body is the spec the planner reads each iteration (ADR-0025).

```yaml
---
goal: Migrate all Go services from logrus to log/slog
provider: gitlab            # gitlab | github
project: acme/example       # path-with-namespace or owner/repo
runner: claude              # see runner.Registry
base_branch: main
concurrency: 1
status: ready               # everflow only acts on "ready" specs
draft_mrs: true             # open MRs as Draft / WIP
---

<markdown body — the planner reads this each iteration>
```

Operational flags on `everflow daemon`: `--listen :8080`, `--public-base-url https://...` (only required if any Run uses `event_source: webhook`; see ADR-0031), `--gitlab-base-url`, `--github-base-url`, `--trigger-listen 127.0.0.1:8081`. Skills live under `~/.everflow/runs/<runID>/SKILL.md` and are mirror-symlinked into the worktree's `.claude/skills/`. Starlark filters default to `internal/filter/default.star`; per-Run overrides land under the Run directory.

## Per-Run filesystem layout

```
~/.everflow/
├── runs/
│   └── <runID>/
│       ├── note_added.star       # Starlark filter for review comments (optional override)
│       ├── pipeline_failed.star  # Starlark filter for CI events (optional override)
│       ├── phrases.yaml          # learned skip phrases (per-Run scope)
│       ├── planning/             # planner artefacts (ADR-0025): spec snapshots, next-increment notes
│       └── worktrees/<unitID>/   # one git worktree per in-flight unit
├── phrases.global.yaml           # cross-Run defaults; human-curated only
├── store.db                      # sqlite RecordStore + TimeoutStore (ADR-0022)
└── daemon.pid
```

## The Runner interface

```go
type Runner interface {
    Name() string
    Run(ctx context.Context, req Request) (Response, error)
}

type Request struct {
    Worktree     string
    SkillCommand string  // "/refactor-logrus-to-slog services/payments"
    Goal         string
    UnitID       string
    UnitContext  string  // bounded — only this unit's scope

    // Replayed inputs for "address comment" / "fix CI" invocations:
    CommentBody  string
    CIFailure    string  // last ~2KB of log

    Timeout time.Duration
    Budget  Budget
}

type Response struct {
    Decision  Decision  // continue | ask | done | fail | nochange
    Summary   string
    Question  string    // populated when Decision == Ask
    Learnings Learnings // { add_phrases, skill_update }
    Tokens    int
}
```

`DecisionNoChange` exists alongside the four canonical decisions for invocations that intentionally produce no diff (e.g. a conversational reply to a reviewer question — see ADR-0034). The workflow keeps the Run in `AwaitingMerge`, posts an info comment, and resolves the thread.

## Provider abstraction

```go
type Provider interface {
    Name() string
    AuthenticatedUser(ctx context.Context) (User, error)

    // Webhook lifecycle (opt-in; ADR-0031).
    RegisterWebhook(ctx context.Context, projectID, callbackURL, secret string, events []EventKind) (webhookID string, err error)
    DeregisterWebhook(ctx context.Context, projectID, webhookID string) error
    VerifySignature(headers http.Header, body []byte, secret string) bool
    NormaliseEvent(headers http.Header, body []byte) (Event, error)

    // MR lifecycle.
    CreateMR(ctx context.Context, projectID string, mr MRDraft) (MR, error)
    PostComment(ctx context.Context, projectID string, mrIID int, body string) error
    UpdateMRTitle(ctx context.Context, projectID string, mrIID int, title string) error
    CloseMR(ctx context.Context, projectID string, mrIID int) error

    // Polling (default event source; ADR-0031).
    GetMRState(ctx context.Context, projectID string, mrIID int) (state string, err error)
    ListNotesSince(ctx context.Context, projectID string, mrIID int, sinceNoteID int64) ([]NotePoll, error)

    // Auto-resolve threads after a runner-driven push (ADR-0034).
    ResolveDiscussion(ctx context.Context, projectID string, mrIID int, discussionID string) error

    RetryPipelineJob(ctx context.Context, projectID string, jobID int64) error
    IsBot(u User) bool
}
```

Both `gitlab.Provider` and `github.Provider` are implemented. `ResolveDiscussion` lands threads automatically on push (GitLab REST, GitHub GraphQL `resolveReviewThread`; see ADR-0034).

## Implementation status

The v1 baseline is in place. The retained v0 scheduled-skill code lives under `_v0/` for reference to the workflow primitives but does *not* drive any current behaviour.

Built and exercised end-to-end (concurrency = 1):

- `internal/provider` — interface, plus `gitlab.Provider` and `github.Provider` implementations (webhook register/verify/normalise, MR create/comment/title, polling via `GetMRState` + `ListNotesSince`, `ResolveDiscussion`).
- `internal/webhook` — HTTP server at `:port/webhook/{provider}/{runID}`, HMAC verify, dispatch to `workflow.Callback` (opt-in; ADR-0031).
- `internal/poller` — 30s ticker, default event source; synthesises `provider.Event` values from REST polls and dispatches via the same callback path (ADR-0031).
- `internal/store` — sqlite-backed `RecordStore` + `TimeoutStore`; daemon restart preserves Runs.
- `internal/eventstream` — in-process `EventStreamer` parked on `sync.Cond`, replacing luno/workflow's busy-spinning `memstreamer` (ADR-0033).
- `internal/refactorsweep` — `Initiated → Working → AwaitingMerge → Paused → AwaitingAbandonConfirm → Completed/Failed/Cancelled` state machine, with author-control commands.
- `internal/filter` — Starlark filter eval (`default.star` ships as the default), per-Run + global phrase files.
- `internal/git` — worktree management plus binary-aware staging (ADR-0032).
- `internal/spec` — markdown-with-YAML-frontmatter spec parser (ADR-0024); spec drives the planner-mode Run (ADR-0025).
- `main.go` — `everflow daemon | start | status | list | phrases | version` CLI; localhost-only trigger HTTP server on `:8081` (ADR-0028).

Still open: concurrency > 1 (parent + child Runs per ADR-0015), and a polling implementation for GitHub (v1 GitHub Runs use webhook mode — see ADR-0031).

## Open questions

These don't block the architecture, but they shape implementation:

1. **Signal that learning is working** — the filter should call the LLM less often over time. What's the measurable signal? A counter of `subagent_invocations / total_events` per Run? Plotted where? Worth thinking through before we ship "the system gets smarter."

2. **Subagent invocation atomicity** — when processing a unit, does one `claude -p` invocation (a) read the skill, (b) make the change, (c) open the MR, (d) update the skill with learnings, in one call? Or are those separate calls? Per-unit-one-call is cheaper; multi-call is more verifiable. Default to one call for v1; reconsider if quality is poor.

3. **Dependencies between units** — some refactors have ordering constraints (unit B depends on unit A being merged first). Do we model this? v1 says no — the discovery rule is responsible for enumerating units that *can* run in any order. If ordering matters, the author writes a discovery rule that returns the next unit when the prior is shipped, not all units up front.

4. **Re-discovery cadence and cost** — does discovery re-run after every merge, or on a timer, or both? Cheap for grep-the-filesystem rules; expensive for "call an API to enumerate." Probably configurable.

5. **Skill mutation safety** — the subagent edits `SKILL.md` after each unit. What if it edits it badly and the next unit's subagent now does worse work? Version it (every change is a git commit on the mirror), and add `everflow skill-history <runID>` so the author can roll back.

6. **Poll-interval tuning under concurrency > 1** — a single in-flight MR uses ~6,000 GitLab API calls/day at the 30s default (ADR-0031). N concurrent MRs scale linearly. When ADR-0015's parent/child Run topology lands, the poll interval (or a coordinated single poll feeding all children) becomes a knob that needs deliberate defaults.
