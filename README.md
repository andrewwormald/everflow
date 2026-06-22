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

**v1 daemon**: implemented. Spec mode (one spec → one Run, agent plans each increment) and sweep mode (mechanical refactor over a static unit list) both work end-to-end. ~3,400 LOC + 30 ADRs.

**v0 (scheduled-skill, archived)**: the original PoC lives under [`_v0/`](_v0/) as a self-contained module. Useful as reference for how `luno/workflow`'s primitives compose; not part of the v1 daemon.

What's working:

- Two modes: **spec-driven** (planner picks each increment) and **sweep** (static unit list)
- **Two providers**: GitLab and GitHub, both with webhook register / verify / event dispatch / MR open / comment / close
- **Claude runner**: shells out to `claude -p`, parses decision marker from response
- **Durable**: sqlite-backed RecordStore + TimeoutStore; survives daemon restart
- **Worktree-isolated git operations**: per-unit worktree + commit + push + open MR
- **MR-comment control surface**: `/everflow pause | resume | skip | retry | prompt | status | stop | abandon`
- **Author privilege model**: control verbs only from the Run's author
- **Abandonment-with-confirmation**: `/everflow abandon` is a two-tap stop with a 12h window

What's not built yet (post-spike):

- Starlark filter integration (currently `StubFilter` returns `InvokeSubagent` on substantive comments + `ControlCommand` on author /everflow)
- `everflow status <runID>` and `everflow list` CLI subcommands
- Qwen / OpenHands runner adapters
- Phrase-learning loop for the filter

## Running a spike

End-to-end run against a real repo. Five steps.

### 1. Prerequisites on the daemon host

- Go 1.26+
- `git` on `$PATH`
- `claude` on `$PATH`, authenticated (i.e. `claude -p "hello"` works)
- A clone of your target repo at a path the daemon can read+write, with an `origin` remote and SSH key / credential helper that lets the daemon push
- A `GITLAB_TOKEN` (or `GITHUB_TOKEN`) env var with API + webhook permissions (`api` scope for GitLab; `repo` + `admin:repo_hook` for GitHub)

### 2. Public URL for inbound webhooks

The provider needs to reach the daemon. Options, in order of preference:

| Host | Recommended tunnel |
|---|---|
| VPS / EC2 / cloud VM | Native public DNS; open the port in your security group |
| Laptop | **Tailscale Funnel** — free, stable `*.ts.net` URL: `tailscale funnel 8080` |
| Laptop fallback | ngrok (free tier rotates URLs; paid is stable) |

### 3. Write a spec

```markdown
---
goal: Replace internal/legacy with internal/v2 across services
provider: gitlab
project: lunomoney/core
runner: claude
base_branch: main
base_repo: /home/ubuntu/dev/core
concurrency: 1
status: ready
---

# Migration plan

For each service still importing `internal/legacy`, switch to
`internal/v2`. Preserve the public function signatures and error
messages.

## Per-unit checklist

- Update the import line
- Run `go test ./...` in the affected package
- Don't touch unrelated code

## Done when

`git grep -l "internal/legacy"` returns nothing under `services/`.
```

Save it somewhere accessible — `~/specs/legacy-migration.md`.

### 4. Start the daemon

```bash
go build -o everflow .

GITLAB_TOKEN=... ANTHROPIC_API_KEY=... \
  ./everflow daemon \
    --public-base-url https://everflow.<user>.ts.net \
    --listen :8080 \
    --trigger-listen 127.0.0.1:8081 \
    --store ~/.everflow/store.db
```

You should see:

```
provider registered  name=gitlab
everflow daemon started  listen=:8080  trigger_listen=127.0.0.1:8081 ...
```

The daemon is now listening for webhooks on `:8080` (public) and triggers on `:8081` (local-only).

### 5. Trigger the spec

In another shell on the same host:

```bash
./everflow start --spec ~/specs/legacy-migration.md
```

You should see:

```
Triggered run <run_id> (foreign id: <foreign_id>, mode: spec)
```

In the daemon's log:

```
triggered run  run_id=...  mode=spec  provider=gitlab  project=lunomoney/core
```

The daemon's `setup` step now: (a) calls GitLab to fetch your authenticated user (Author), (b) generates a webhook secret, (c) registers a webhook at `https://everflow.<user>.ts.net/webhook/gitlab/<runID>`, (d) creates `~/.everflow/runs/<runID>/`. Then `discover` invokes the claude planner with the spec body to choose the first increment. Then `work` runs claude in a fresh worktree off `main`, commits + pushes, opens the MR, posts an initial status comment, and parks in `AwaitingMerge`.

### Driving it via MR comments

Once an MR is open, reply on it:

```
/everflow status            → daemon posts a progress summary
/everflow pause             → daemon stops reacting until /resume
/everflow skip too risky    → close this MR, pick the next increment
/everflow prompt focus on tests first
                            → next runner call gets your text prepended
/everflow stop              → one-tap cancel: close MRs, end the Run
/everflow abandon           → two-tap cancel: posts "are you sure?";
                              second /everflow abandon within 12h confirms
```

Any other comment, the runner addresses (via the StubFilter → claude) and pushes a follow-up commit. CI failures trigger the same path.

### Known limitations of the spike

- **StubFilter** invokes the subagent on every substantive comment. A future Starlark filter will let you skip "lgtm" etc deterministically.
- **No `everflow status` CLI** yet — use `/everflow status` on an MR comment, or `sqlite3 ~/.everflow/store.db "SELECT * FROM records"`.
- **Single concurrency** in v1; the throttled-sequential model parallelises in v2.
- **No retry-replay**: `/everflow retry` clears the pause but the author has to re-trigger by event (re-comment, wait for CI rerun).

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
| `main.go` | CLI entrypoint: `daemon`, `start`, `status` (stub), `version`, `runners` |
| [`internal/refactorsweep/`](internal/refactorsweep/) | The state machine + step bodies + control verbs |
| [`internal/provider/{gitlab,github}/`](internal/provider/) | Platform adapters |
| [`internal/runner/claude/`](internal/runner/claude/) | Claude shell-out runner with decision-marker parsing |
| [`internal/git/`](internal/git/) | `git` CLI wrapper |
| [`internal/store/`](internal/store/) | Sqlite-backed `workflow.RecordStore` + `TimeoutStore` |
| [`internal/spec/`](internal/spec/) | Spec markdown parser (frontmatter + body) |
| [`internal/webhook/`](internal/webhook/) | HTTP webhook ingress |
| [`internal/filter/`](internal/filter/) | Event filter (StubFilter today; Starlark planned) |
| [`_v0/`](_v0/) | Archived scheduled-skill PoC, separate module |

## License

MIT
