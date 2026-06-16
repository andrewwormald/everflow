<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="logo/mark-dark.gif">
    <img src="logo/mark.gif" alt="Everflow" width="220" />
  </picture>

  <h1>everflow</h1>

  <p><strong>Crunch through bulk refactors across large codebases.</strong><br/>One MR at a time, or ten — without burdening the author or overwhelming reviewers.</p>
</div>

---

Everflow drives **bulk refactor sweeps**: open an MR for one unit of work, watch it through review and CI, address feedback, ship the merge, pick up the next unit, repeat until done. Configurable concurrency (default 1) controls how many MRs are in flight at any time, so reviewers aren't drowned and merge conflicts don't pile up. The author doesn't have to babysit anything; they get pinged on the MR if everflow gets stuck.

Built on [`luno/workflow`](https://github.com/luno/workflow) for the durable state machine. Designed to run on a small VPS or EC2 instance, not your laptop.

## What problem it solves

Standardising a pattern across a large monorepo is *almost* repetitive — same shape of change, 47 services. Almost.

The current options are bad:

- **Open all 47 MRs at once** — overwhelms reviewers, creates a merge-conflict bomb (each merge invalidates the others), creates social debt the team has to grind through
- **Manually crank one at a time** — wastes your day. You're the bottleneck between "MR N just merged" and "open MR N+1." You can't go on holiday mid-refactor.
- **Have Claude Code do it interactively** — your terminal session dies, the context blows up, and you're paying tokens for the repetitive "check status, retry, classify, move on" work that doesn't need reasoning.

Everflow is what's missing: a daemon that handles the cheap repetitive parts deterministically (queueing, throttling, status reporting, classifying review comments, retrying flakes) and only spawns a bounded-context subagent for the work that genuinely needs reasoning. **The workflow runs cheaply; LLM tokens fire only when they earn their keep.**

## How it works

```
  Discover units                                                   ┌── merged ──► next unit
        │                                                          │
        ▼                                                          │
  Working ─► open MR ─► Awaiting-merge (idle, zero cost, days OK) ─┤
                              │                                    │
                              ├── reviewer comment ─► filter ──────┤
                              │       │                            │
                              │       └── substantive? subagent  ──┘
                              │       └── emoji/lgtm? skip         (loop)
                              │
                              ├── CI failed ─► classify ──► known flake: retry
                              │                       └──► novel: subagent
                              │
                              └── author /everflow command ──► state transition
```

- The **MR comment thread is the only communication channel** ([ADR-0016](decisions/0016-mr-comments-only-channel.md)). Everflow posts status updates; you reply with `/everflow pause`, `/resume`, `/skip`, `/retry`, `/prompt …`, `/status`, or `/stop`.
- A **Starlark filter** runs on every event ([ADR-0018](decisions/0018-starlark-filter-and-phrase-learning.md)). Cheap deterministic classification first; subagent only when needed. The filter learns ("`lgtm` → skip in future") within bounds.
- **Author privileges**: control commands work only from the user who triggered the Run ([ADR-0017](decisions/0017-author-privilege-model.md)). Reviewers can't accidentally skip an MR by typing the wrong thing.
- **Webhook-driven**: workflow sleeps idle for hours/days between events, zero compute cost. Polling is a fallback for when webhooks haven't fired in a while.

Full design: [`DESIGN.md`](DESIGN.md). The decisions log under [`decisions/`](decisions/) records every meaningful trade-off.

## Status

**v1 design**: complete. See [`DESIGN.md`](DESIGN.md).
**v1 implementation**: not yet — see DESIGN.md § *What's not yet built* for the build order.

**v0 (reference, in this repo)**: a scheduled-skill daemon that demonstrates the underlying workflow primitives (durable Run state, worktree isolation, runner abstraction, AddTimeout-driven cycles). Useful as a sanity check that the workflow library fits the use case; not the headline product. See [ADR-0010](decisions/0010-scheduled-skill-poc-first.md).

If you want to see the workflow library in action right now, the v0 code builds and runs (see *v0 quickstart* below). For the actual v1 refactor-sweep flow, follow this repo for updates.

## What's planned

In rough build order:

1. **GitLab Provider adapter** — webhook register/dispatch, MR create/comment, signature verify
2. **HTTP server in the daemon** — `:port/webhook/...`, HMAC, workflow.Callback dispatch
3. **Sqlite store** — daemon restart preserves Runs
4. **Refactor-sweep state machine** — `Discovering → Working → Awaiting-merge → ...`
5. **Starlark filter integration** — per-event evaluation, phrase-learning loop
6. **Control command handler** — author-only `/everflow ...` in MR comments
7. **`everflow start` CLI** + `status`, `phrases promote`
8. **GitHub Provider adapter** — second implementation to validate the abstraction

Concurrency > 1 (parent/child Runs) is v2.

## v0 quickstart (current code, scheduled-skill flow)

Demonstrates the durable workflow plumbing. Not the v1 product. Useful for poking at the workflow library shape.

```bash
go build -o everflow .
./everflow runners                          # → claude, mock

./everflow \
    --skill   "/dummy"           \
    --runner  mock               \
    --interval 5s                \
    --base-repo ~/dev/some-repo  \
    --root    /tmp/everflow-wt
```

You'll see the daemon trigger a Run, set up a worktree off `main`, and cycle through `Idle → Running` on the interval. Ctrl-C to stop. See [v0 archive notes in DESIGN.md](DESIGN.md#whats-not-yet-built) for context.

## Running on a server

The natural home is a small VPS or EC2 instance, not your laptop. Webhooks need a stable public URL, and the workflow needs to keep ticking while you sleep — a server gives you both for free.

```ini
# /etc/systemd/system/everflow.service
[Unit]
Description=everflow daemon
After=network.target

[Service]
Type=simple
User=ubuntu
WorkingDirectory=/home/ubuntu
ExecStart=/usr/local/bin/everflow daemon \
    --public-base-url https://everflow.example.com
Restart=on-failure
RestartSec=10s
Environment=ANTHROPIC_API_KEY=...
Environment=GITLAB_TOKEN=...

[Install]
WantedBy=multi-user.target
```

Laptop deployment is supported but adds tunnel friction — Tailscale Funnel works best (free, stable URL on `*.ts.net`); ngrok free works with the caveat that URLs rotate; cloudflared works if you have a domain. See [`DESIGN.md`](DESIGN.md#public-url-strategy).

## Repository layout

| Path | Purpose |
|---|---|
| [`README.md`](README.md) | This file |
| [`DESIGN.md`](DESIGN.md) | Full architecture and roadmap |
| [`AGENTS.md`](AGENTS.md) | Working rules for AI contributors (read this if you're an agent picking up the repo) |
| [`decisions/`](decisions/) | Architecture Decision Records — every meaningful choice and its rationale |
| [`logo/`](logo/) | Brand assets (GIF + SVG variants, dark-mode pair, favicon) |
| `*.go` | v0 scheduled-skill implementation (reference code; v1 will largely replace) |

## License

MIT
