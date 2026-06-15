# Everflow

> Durable, terminal-independent runner for Claude Code / Qwen Code / OpenHands skills.

Everflow runs your AI coding agent on a schedule, in an isolated git worktree, in a durable workflow that survives terminal closure, daemon restarts, and machine reboots. Designed for the work that doesn't fit a single session — multi-day CI babysitting, scheduled audits, "keep this MR green until it merges."

Built on [`luno/workflow`](https://github.com/luno/workflow) for the durable state machine.

## What problem it solves

Interactive AI coding tools die when their session dies. Useful long-running work — sweep all my open MRs every 30 minutes, retry CI failures, request reviewers, escalate when stuck — needs the agent loop to keep running while no human is at the keyboard.

The current options are:

- **`/loop 30m /my-skill`** in Claude Code — requires Claude Code to stay open
- **A shell script in cron** — loses the agent's context, no audit trail, no resumable state
- **A custom workflow engine** — months of work

Everflow is the missing middle: a small Go binary that gives any Claude Code skill a durable, scheduled, terminal-independent host.

## Status

Implemented (this repo):

- **Scheduled-skill loop**: `Initiated → Idle ⇄ Running` cycle driven by `AddTimeout`, runs your skill at a fixed interval
- **Worktree isolation**: each Run gets a `git worktree` off your base repo; the skill cannot touch your main checkout
- **Pluggable Runner**: `claude` (shells out to `claude -p`) and `mock` (no-op) ship by default; adding a new runner is implementing one interface
- **Single-binary daemon**: run under `launchd`, `systemd`, `tmux`, or `nohup` — no service to install, no database to provision

Designed but not yet built (see [`DESIGN.md`](DESIGN.md)):

- **Interactive `Iterating`/`Awaiting` loop** — for agents that need to ask the user and resume
- **Persistent store** — current build uses in-memory adapters; daemon restart loses Runs
- **`qwen` and `openhands` runners**
- **Webhook callbacks** — react to GitLab/GitHub events within seconds instead of polling

## Install

Requires Go 1.26+ and `git`. For the `claude` runner, also `claude` on `$PATH`.

```bash
go install github.com/andrewwormald/everflow@latest
```

Or from source:

```bash
git clone https://github.com/andrewwormald/everflow
cd everflow && go build -o everflow .
```

## Quickstart — mock runner (no API spend)

Validates the loop mechanics without invoking any LLM.

```bash
everflow \
    --skill   "/dummy"          \
    --runner  mock              \
    --interval 5s               \
    --base-repo ~/dev/some-repo
```

You'll see the daemon trigger a Run, set up a worktree, and start cycling:

```
triggered run 822a8d11... (foreign id: dummy-1781530647)
  skill:    /dummy
  runner:   mock
  interval: 5s
  base:     /Users/you/dev/some-repo @ main
  worktree: /Users/you/.everflow/wt/dummy-1781530647 (branch wf-dummy-1781530647)
press Ctrl-C to stop
[822a8d11] setup worktree at ... (branch ..., base main)
[822a8d11] pass #0 starting (skill: /dummy)
[822a8d11] pass #0 done (exit 0, 1s)
[822a8d11] pass #1 starting (skill: /dummy)
...
```

Ctrl-C to stop. Worktree is left for inspection — remove with `git -C ~/dev/some-repo worktree remove --force ~/.everflow/wt/dummy-*`.

## Quickstart — real `claude` runner

Run an actual Claude Code skill every 30 minutes:

```bash
everflow \
    --skill     "/mrs-babysit"   \
    --runner    claude           \
    --interval  30m              \
    --base-repo ~/dev/core
```

The skill must be available in the base repo — typically under `.claude/skills/<skill-name>/SKILL.md`. Each pass: `git fetch && reset --hard origin/main` in the worktree, then `claude -p "/mrs-babysit" --dangerously-skip-permissions`.

## Running on EC2 / a server

Same binary, same flags. Wrap in a `systemd` unit:

```ini
# /etc/systemd/system/everflow.service
[Unit]
Description=everflow scheduled skill loop
After=network.target

[Service]
Type=simple
User=ubuntu
WorkingDirectory=/home/ubuntu
ExecStart=/usr/local/bin/everflow --skill "/mrs-babysit" --runner claude --interval 30m --base-repo /home/ubuntu/dev/core
Restart=on-failure
RestartSec=10s
Environment=ANTHROPIC_API_KEY=...

[Install]
WantedBy=multi-user.target
```

**Caveat for headless use**: Claude Code skills that rely on `mcp__claude_ai_*` MCP servers (Slack, Atlassian, Google, ...) authenticate via claude.ai's OAuth, which does not work on a headless box. The CI-management half of `mrs-babysit` (`glab` + GitLab MCP via API token) works; the Slack/Jira half does not.

## Command reference

```
everflow runners
    List registered runners and exit.

everflow [flags]
    Start the daemon, trigger a Run, block on signals. Ctrl-C / SIGTERM to stop.

Flags:
  --skill        slash command to invoke each pass, e.g. /mrs-babysit
  --runner       claude | mock (default: claude)
  --interval     5m, 30m, 1h, ... (default: 30m)
  --base-repo    absolute path to the git repo to base the worktree off (required)
  --base-branch  branch to base the worktree off and reset to each pass (default: main)
  --root         where to store worktrees (default: ~/.everflow/wt)
  --id           foreign ID for the Run (default: auto-generated from skill+timestamp)
```

## How it works

```
                 ┌─────────────┐
   Trigger ───►  │  Initiated  │
                 └──────┬──────┘
                        │ setupStep (creates worktree off origin/main)
                        ▼
                 ┌─────────────┐
              ┌─►│    Idle     │◄────────────┐
              │  └──────┬──────┘             │
              │         │ AddTimeout +Interval│
              │         ▼                    │
              │  ┌─────────────┐             │
              │  │   Running   │─runPass─────┘
              │  └─────────────┘
              │         │
              └─────────┘
        cycle continues until process stops
```

Each `runPass`:

1. `git fetch origin && git reset --hard origin/<base-branch>` — fresh starting point
2. Invoke the configured `Runner` with `(worktree, skill_command, timeout)`
3. Append a `Turn` to the Run's history; return to `Idle` and re-arm the interval timer

See [`DESIGN.md`](DESIGN.md) for the full architecture, the planned interactive loop, and the `Runner` interface roadmap.

## Repository layout

| File | Purpose |
|---|---|
| `main.go` | CLI flag parsing, daemon entrypoint, signal handling |
| `agent.go` | `AgentState`, `AgentStatus`, workflow builder, step functions |
| `runner.go` | `Runner` interface, registry |
| `claude.go` | `claude -p` runner |
| `mock.go` | No-op runner for demos and tests |
| `worktree.go` | `git worktree add`, `fetch && reset --hard`, removal |
| `DESIGN.md` | Full design doc — vision, future runners, webhook plans |

## License

MIT
