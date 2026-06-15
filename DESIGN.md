# Everflow — design

Long-running AI agent loops on `luno/workflow`. A durable, terminal-independent
runner for Claude Code / Qwen Code / OpenHands skills.

**Status**: Design doc. The scheduled-skill subset is implemented; the full
`Iterating`/`Awaiting` loop is not.
**Author**: Andrew Wormald
**Last updated**: 2026-06-15

## Motivation

Interactive AI coding tools (Claude Code, OpenHands, Codex CLI) are bound to a single session: when the user closes the terminal, the agent loop dies. Stateful long-running work — multi-day refactors, scheduled audits, "babysit this until CI is green" — needs an L1/L2/L3 model:

- **L1 Setup & Goal** — user states a goal once
- **L2 Agentic Loop** — Plan → Act → Observe → Reflect, repeating for hours/days
- **L3 Stateful Memory** — progress survives process restarts, machine reboots, and human absence

`luno/workflow` already provides every primitive L3 needs (durable `RecordStore`, event log, `Callback`/`Timeout` for waiting without busy loops, `Pause` for stuck runs, role scheduling for horizontal scale, web UI for inspection). This proposal is a thin CLI — `everflow` — that wires those primitives into the L1/L2/L3 model and ships as a **Skill** that existing AI coding tools invoke.

This CLI is not a replacement for Claude Code or OpenHands. Those tools remain the user's primary interface. They *trigger* `everflow`, query its state, and feed answers back when the loop pauses for input.

## L1/L2/L3 → workflow mapping

| Agent concept | `luno/workflow` primitive |
|---|---|
| Goal statement (L1) | `Trigger(foreignID, WithInitialValue(&AgentState{Goal: "..."}))` |
| Conversation/turn history (L1+L3) | `Run.Object.History []Turn` — durable, survives restarts |
| Iterate loop (L2) | Single `Iterating` step that cycles to itself; the runner decides per-invocation whether it did one turn or many |
| Wait on external tool / human (L2) | `AddCallback` at a "needs input" status |
| Wake up on a schedule (L2) | `AddTimeout` — "rerun this step in 4h if no progress" |
| Resumable memory (L3) | `RecordStore`; Run identity = `(workflowName, foreignID, runID)` |
| Stuck-run escape valve | `Run.Pause()` → `RunStatePaused` → human resolves via callback |
| Multiple concurrent agents | Each agent = one Run; `ParallelCount` shards step workers |
| Audit trail | Event log (every status transition is a durable event) |

## Architecture

```
                       ┌──────────────────────┐
   user / claude  ───► │  everflow CLI  │  ◄─── everflow daemon
                       │  (start/status/...)  │       (long-lived process)
                       └──────────┬───────────┘                │
                                  │                            │
                                  ▼                            ▼
                         ┌─────────────────────────────────────────┐
                         │  luno/workflow runtime                  │
                         │  - RecordStore (sqlite for v1)          │
                         │  - EventStreamer (in-process for v1)    │
                         │  - TimeoutStore (sqlite for v1)         │
                         └────────────────┬────────────────────────┘
                                          │ per step
                                          ▼
                         ┌─────────────────────────────────────────┐
                         │  Step executor                          │
                         │  - cd to ~/.everflow/wt/<runID>/  │
                         │  - invoke configured Runner with Run    │
                         │    state (goal, history, scratchpad)    │
                         │  - parse runner's structured output     │
                         │  - return next Status                   │
                         └────────────────┬────────────────────────┘
                                          │
                                          ▼
                         ┌─────────────────────────────────────────┐
                         │  Runner (per-Run, --runner flag)        │
                         │  - claude    → claude -p --output-format json
                         │  - qwen      → qwen -p (JSON via prompt)│
                         │  - openhands → openhands headless -t    │
                         └─────────────────────────────────────────┘
```

Decisions baked in (from prior discussion):

- **Single long-lived daemon** (`everflow daemon`), not cron-driven ticks. The user runs it under `launchd`/`systemd` (or just leaves it in a tmux pane). It calls `wf.Run(ctx)` and processes events for the `agent-loop` workflow.
- **Pluggable Runner** chosen at `start` time via `--runner` (claude | qwen | openhands). The workflow definition is shared; only the runner differs per Run. See [Runners](#runners) below.
- **Context lives in the workflow Run**, not in a long-lived agent subprocess. Each runner invocation is a pure function of `(goal, history, scratchpad, answer)` → structured response. This is what makes L3 fall out for free — resumption is just "rehydrate the Run and re-invoke the runner."
- **One git worktree per Run**, created at `Trigger`, destroyed (or kept for inspection) at `Completed`/`Cancelled`. The worktree is the runner's filesystem sandbox.

## The Run shape

```go
type AgentState struct {
    Goal       string    // L1 — the user's high-level goal, set once at Trigger
    Worktree   string    // absolute path to ~/.everflow/wt/<runID>/
    Branch     string    // wf-<runID>
    BaseBranch string    // usually "main"

    RunnerName string    // "claude" | "qwen" | "openhands" — set at Trigger, immutable after
    Budget     Budget    // MaxTurns, MaxTokens; enforced in iterate before invoking runner

    History    []Turn    // append-only log of every runner invocation
    Scratchpad string    // model-updatable durable notes; persists across turns
    Question   string    // when paused: the question the runner is asking the human
    Answer     string    // when resumed: the human's answer, fed back into next invocation

    LastError  string
    Stats      Stats     // tokens used, turns taken, wall-clock elapsed
}

type Budget struct {
    MaxTurns  int   // hard stop after N runner invocations
    MaxTokens int   // hard stop after N total tokens across the Run
}

type Turn struct {
    Index     int
    Runner    string    // "claude" | "qwen" | "openhands"
    Summary   string    // human-readable what-the-runner-did this invocation
    StartedAt time.Time
    EndedAt   time.Time
    Tokens    int
}

type AgentStatus int
const (
    AgentStatusUnknown   AgentStatus = 0
    AgentStatusInitiated AgentStatus = 1   // Run created, worktree not yet built
    AgentStatusIterating AgentStatus = 2   // runner is doing work (or about to be invoked again)
    AgentStatusAwaiting  AgentStatus = 3   // paused, waiting on human via callback
    AgentStatusCompleted AgentStatus = 4   // goal reached; branch ready to PR
    AgentStatusFailed    AgentStatus = 5   // unrecoverable; worktree kept for forensics
)
```

## The step graph

```go
b := workflow.NewBuilder[AgentState, AgentStatus]("agent-loop")

// Build the worktree, then enter the loop
b.AddStep(AgentStatusInitiated, setupWorktree, AgentStatusIterating)

// L2 loop — Iterating cycles to itself until the runner decides otherwise
b.AddStep(AgentStatusIterating, iterate,
    AgentStatusIterating,  // continue the loop
    AgentStatusAwaiting,   // runner needs human input
    AgentStatusCompleted,  // goal reached
    AgentStatusFailed,     // unrecoverable
)

// Human-in-the-loop: everflow resolve <runID> --input "..." fires this
b.AddCallback(AgentStatusAwaiting, resumeFromAnswer, AgentStatusIterating)

// Safety timeout — if Iterating sits for more than the runner's configured
// max-invocation wall-clock, pause for inspection. OpenHands runners get a
// longer ceiling than claude/qwen runners.
b.AddTimeout(AgentStatusIterating, runnerTimeoutTimer, timeoutToAwaiting, AgentStatusAwaiting)
```

The single `iterate` step is short: it resolves the runner from `Run.Object.RunnerName`, calls `Runner.Run(ctx, RunRequest{...})` with the durable Run state, appends a `Turn`, and maps the runner's `Decision` field onto the next workflow status. The runner decides per-invocation whether it did one logical turn or twenty.

### What `iterate` looks like (sketch)

```go
func iterate(ctx context.Context, r *workflow.Run[AgentState, AgentStatus]) (AgentStatus, error) {
    runner, err := runners.Get(r.Object.RunnerName)
    if err != nil {
        return AgentStatusFailed, err
    }

    resp, err := runner.Run(ctx, RunRequest{
        Worktree:   r.Object.Worktree,
        Goal:       r.Object.Goal,
        History:    r.Object.History,
        Scratchpad: r.Object.Scratchpad,
        Answer:     r.Object.Answer, // populated by resumeFromAnswer if resuming
        Budget:     r.Object.Budget,
    })
    if err != nil {
        return AgentStatusFailed, err
    }

    r.Object.Answer = "" // consume the answer
    r.Object.Scratchpad = resp.Scratchpad
    r.Object.History = append(r.Object.History, Turn{
        Index:     len(r.Object.History),
        Runner:    runner.Name(),
        Summary:   resp.Summary,
        StartedAt: resp.Start, EndedAt: resp.End, Tokens: resp.Tokens,
    })

    switch resp.Decision {
    case DecisionContinue:
        return AgentStatusIterating, nil
    case DecisionAsk:
        r.Object.Question = resp.Question
        return AgentStatusAwaiting, nil
    case DecisionDone:
        return AgentStatusCompleted, nil
    case DecisionFail:
        r.Object.LastError = resp.Summary
        return AgentStatusFailed, nil
    }
    return AgentStatusFailed, fmt.Errorf("unknown decision: %v", resp.Decision)
}
```

The runner never sees the workflow runtime. The workflow never sees the LLM. The `Runner` interface is the only boundary.

## Runners

A `Runner` is the integration point between workflow and a coding agent. v1 ships three.

```go
type Runner interface {
    Name() string                                          // "claude" | "qwen" | "openhands"
    Run(ctx context.Context, req RunRequest) (RunResponse, error)
}

type RunRequest struct {
    Worktree   string
    Goal       string
    History    []Turn        // prior turns this Run has done
    Scratchpad string        // durable model notes
    Answer     string        // if non-empty, this is a resume from Awaiting
    Budget     Budget        // per-invocation ceiling: tokens, wall-clock
}

type RunResponse struct {
    Decision   Decision      // continue | ask | done | fail
    Summary    string        // one-paragraph "what I did this invocation"
    Scratchpad string        // updated durable notes (Run.Object.Scratchpad := this)
    Question   string        // populated when Decision == "ask"
    Tokens     int
    Start, End time.Time
}

type Decision int
const (
    DecisionContinue Decision = iota
    DecisionAsk
    DecisionDone
    DecisionFail
)
```

### Built-in implementations

| Runner | Transport | Output parsing | Per-invocation wall-clock |
|---|---|---|---|
| `claude` | `exec.Command("claude", "-p", "--output-format", "json", ...)` | Native — Claude Code's JSON output mode | 10 min default |
| `qwen` | `exec.Command("qwen", "-p", ...)` | Native if Qwen Code's CLI exposes structured output; otherwise prompt-suffix JSON contract | 10 min default |
| `openhands` | `exec.Command("openhands", "headless", "-t", goal, ...)` plus reading OpenHands' session state file | Native — OpenHands writes structured session state to disk | 60 min default (it runs a whole subtask per invocation) |

Each runner is a small package under `runners/` with its own command-line construction, environment setup (env-var passthrough for API keys), and output parser. Adding a new runner = implementing the interface and registering it in a `Registry` map.

### Configuration

A single YAML file at `~/.everflow/config.yaml` overrides defaults:

```yaml
default_runner: claude
runners:
  claude:
    command: claude
    extra_args: ["--model", "claude-opus-4-7"]
    timeout: 10m
  qwen:
    command: qwen
    extra_args: ["--model", "qwen3-coder"]
    timeout: 10m
  openhands:
    command: openhands
    extra_args: ["--llm-model", "anthropic/claude-sonnet-4-6"]
    timeout: 60m
```

Per-Run override at `start` time: `everflow start --runner qwen --goal "..."`. The runner name is stored on the Run, so the same daemon can have claude, qwen, and openhands runs in flight simultaneously.

## Isolation: worktree per Run

Created at `Trigger` (inside `setupWorktree`):

```bash
git worktree add -b wf-<runID> ~/.everflow/wt/<runID>/ <baseBranch>
```

Inside that directory, the configured runner runs in yolo mode (`claude --dangerously-skip-permissions`, `openhands` with confirmation disabled, etc. — each runner's adapter handles the equivalent flag). Yolo mode is acceptable because:

1. The user pre-authorized the agent by triggering the workflow with a goal
2. Writes can only land in the worktree dir, not the user's main checkout
3. Commits land on `wf-<runID>`, never on `main`
4. The CLI explicitly does **not** grant credentials for `gh`, network deploys, `kubectl`, etc. — those remain off-limits unless the user adds them to the daemon's environment

When the runner needs something it cannot do autonomously (a credential, a design decision, a sanity check), it returns `Decision: DecisionAsk` with a populated `Question` field. The Run transitions to `AgentStatusAwaiting`; the user is notified (initially just via `everflow list --awaiting`, later via Slack/desktop notifications). The user answers via:

```
everflow resolve <runID> --input "use staging API, key is in 1Password"
```

That fires `workflow.Callback`, which runs `resumeFromAnswer`: it copies `--input` into `Object.Answer`, transitions back to `AgentStatusIterating`, and the next `iterate` invocation passes the answer to the runner.

## CLI surface

```
everflow daemon [--store sqlite:~/.everflow/db]
    Start the long-lived process. Idempotent; second invocation exits cleanly.

everflow start --goal "<goal>" [--base main] [--runner claude|qwen|openhands]
                     [--max-turns 50] [--max-tokens 500000]
    Create a Run. Builds the worktree, prints the runID. --runner defaults
    to the value in ~/.everflow/config.yaml.

everflow runners
    List configured runners and their effective settings (command, model,
    timeout). Useful for "is the openhands binary actually on my PATH?"

everflow status <runID>
    Current AgentStatus, branch, worktree path, last 5 turns, current Question
    if Awaiting.

everflow list [--mine] [--awaiting] [--state running|paused|completed]
    Tabular list. --awaiting is the "things that need me" filter.

everflow resolve <runID> --input "<answer>"
    Answer a paused agent's question. Unblocks the Run.

everflow logs <runID> [--phase plan|act|observe|reflect] [--tail]
    Stream Turn history. --tail follows live as the daemon emits new turns.

everflow stop <runID> [--reason "..."]
    Cancel a running agent. Worktree is kept for inspection until purge.

everflow finalize <runID>
    For Completed runs: prints the gh pr create command (does not run it).
    Includes a drift summary (commits behind base).

everflow purge <runID>
    Remove the worktree and branch. Run record stays for audit.
```

## Skill integration

Distribution is a Claude Code Skill so adoption requires no behavior change from the user — Claude Code discovers it the moment they install the skill bundle.

```
~/.claude/skills/everflow/SKILL.md
~/.claude/skills/everflow/scripts/...
```

`SKILL.md` outline:

```markdown
---
name: everflow
description: Use when the user asks for a multi-hour or multi-day autonomous task
  (refactor, audit, "babysit this", "keep trying until X"). Spawns a durable agent
  loop that survives session restarts.
---

# When to use
- Task estimated > 30 min OR user says "in the background", "overnight", "until it's done"
- Task is bounded and verifiable (the agent can self-check progress)
- Task does NOT need credentials beyond what's already in the worktree

# How to use
1. `everflow start --goal "<paraphrased user goal>"` — capture the runID
2. Tell the user the runID and that they can check in any time via
   `everflow status <runID>` or just ask you
3. When re-invoked, run `everflow list --mine` to see what's in flight
4. If status is `awaiting`: show the user the Question, get an answer,
   call `everflow resolve <runID> --input "<answer>"`
5. When status is `completed`: run `everflow finalize <runID>` and walk
   the user through the PR
```

The runID does not need to live in a persistent context file — `everflow list --mine` is the source of truth. (`--mine` filters by the invoking user via `$USER` + a `created_by` column.)

## Drift handling (v1: don't care)

Long-running workflows fall behind `main`. v1 does nothing — `finalize` prints a one-line summary (`Behind main by 14 commits, 3 files conflict`) and the human resolves at PR time. v2 may add an auto-rebase step on a timer that pauses to `Awaiting` on conflict.

## What this is not

- **Not** a replacement for Claude Code's interactive loop. Short tasks stay interactive.
- **Not** a multi-tenant service. v1 is single-user, single-machine. The daemon binds to a local socket / sqlite file.
- **Not** an LLM router or a tool dispatcher. The runner (claude/qwen/openhands) already does both. We give it a durable, resumable loop around it. Adding a new runner is implementing the `Runner` interface — about 100 lines of glue per runner.

## Open questions (resolve during spike)

1. **Prompt rendering** — what's the minimal context we send per invocation? Goal + last N turn summaries + scratchpad, or do we re-send all turn summaries once they grow? Each runner has different context windows; the runner adapter may need to truncate. (Different per runner — claude has 200k+, qwen and openhands depend on the underlying model.)
2. **Qwen structured output** — does Qwen Code's CLI expose a `--output-format json` equivalent? If not, the qwen runner falls back to a prompt-suffix JSON contract ("end your response with `<workflow-decision>{...}</workflow-decision>`") and a tolerant parser. To verify during spike.
3. **OpenHands invocation shape** — does `openhands headless -t` block until the session ends, and where does it write session state? Spike-time verification; the openhands runner adapter shape depends on this.
4. **Notification surface** — `everflow list --awaiting` is the v1 surface for "agent needs you." Slack/desktop notifications via a `Hook` are v2.
5. **Worktree GC** — purged at the user's command? Or auto-purge Completed runs after N days?
6. **Web UI** — the existing `adapters/webui` already visualizes Runs. We get it for free, but should it be auto-launched by `daemon` or opt-in?
7. **Runner-aware budgets** — `--max-turns` and `--max-tokens` are per-Run. But one openhands turn does much more than one claude turn. Do we report budgets in tokens (apples-to-apples) or in turns (intuitive but unfair across runners)? Probably tokens, with a `--max-turns` safety stop as a backstop.

## Spike plan (next task)

Land at the repo root with:

- `main.go` — wires the workflow with in-memory adapters (sqlite store is post-spike)
- `agent.go` — the `AgentState` type, `AgentStatus` enum, and `iterate` step
- `runner.go` — the `Runner` interface, `Decision` enum, registry
- `runners/claude/claude.go` — the `claude -p --output-format json` adapter
- `runners/qwen/qwen.go` — the qwen adapter (uses prompt-suffix JSON contract until native is confirmed)
- `runners/openhands/openhands.go` — the openhands headless adapter
- `worktree.go` — `git worktree add/remove` helpers
- `cmd/` — `start`, `status`, `resolve`, `runners` subcommands (skip `daemon` for the spike; run inline)
- A README walking through a 5-minute demo: "ask the claude runner to add a test, then re-run with --runner qwen, watch both loops"

Goal of the spike: prove the loop runs end-to-end against at least the claude runner, the worktree boundary holds, the Paused→Resolved round-trip works, and the `Runner` interface is the right shape (verified by getting a second runner — qwen or openhands — to drive the same workflow). Production hardening (sqlite store, launchd plist, Skill bundle, drift summary, notifications) is post-spike.
