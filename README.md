<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="logo/mark-dark.gif">
    <img src="logo/mark.gif" alt="Everflow" width="220" />
  </picture>

  <h1>everflow</h1>

  <p><strong>Crunch through bulk refactors across large codebases.</strong><br/>One MR at a time, or ten — without burdening the author or overwhelming reviewers.</p>
</div>

---

Everflow drives **bulk refactor sweeps**: open an MR for one unit of work, watch it through review and CI, address feedback (auto-resolving the comment thread once the change is pushed), ship the merge, pick up the next unit, repeat until done. Configurable concurrency (default 1) controls how many MRs are in flight at any time, so reviewers aren't drowned and merge conflicts don't pile up. The author doesn't have to babysit anything; they get pinged on the MR if everflow gets stuck.

Built on [`luno/workflow`](https://github.com/luno/workflow) for the durable state machine. Runs anywhere a Go binary runs — laptop, VPS, cloud VM. No public URL required by default.

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
- **Event-driven**: poll the provider by default (zero LLM cost; ADR-0031), or register a webhook if you have a stable public URL. Either way the workflow sleeps idle between events for hours/days — LLM tokens fire only when a comment, CI failure, or merge actually arrives.
- **Threads auto-resolve on push**: when the runner addresses a reviewer comment and lands the change, the discussion thread is marked resolved automatically (ADR-0034). The reviewer sees the conversation close itself.

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

- `everflow status <runID>` HTTP read API (today: query sqlite directly or comment `/everflow status` on the MR)
- Qwen / OpenHands runner adapters (interface exists; only Claude is wired)
- `Provider.ResolveDiscussion` is a no-op on GitHub pending GraphQL `resolveReviewThread` work — GitLab resolves cleanly today (ADR-0034)
- CLI commands for abandon/resume on stuck Runs (today: comment on the MR)

## Running a spike

End-to-end run against a real repo. Five steps.

### 1. Prerequisites on the daemon host

- Go 1.26+
- `git` on `$PATH`
- `claude` on `$PATH`, authenticated (i.e. `claude -p "hello"` works — interactive Claude Code login is fine; no Anthropic API key required)
- A clone of your target repo at a path the daemon can read+write, with an `origin` remote and SSH key / credential helper that lets the daemon push
- Provider auth: either `GITLAB_TOKEN` (API scope) / `GITHUB_TOKEN` (`repo` scope), **or** an existing `glab auth login` — everflow reads the OAuth token from glab's config if no env var is set. For poll-mode (default) no webhook permissions are required.

### 2. Public URL (optional — webhook mode only)

In poll mode (the default — ADR-0031), everflow needs no public URL: the daemon polls the provider's API for changes on each tick. Skip this section unless you specifically want webhook ingress.

If you do want webhook mode (lower latency, requires a stable URL):

| Host | Recommended tunnel |
|---|---|
| VPS / EC2 / cloud VM | Native public DNS; open the port in your security group |
| Laptop | **Tailscale Funnel** — free, stable `*.ts.net` URL: `tailscale funnel 8080` |
| Laptop fallback | ngrok (free tier rotates URLs; paid is stable) |

Then pass `--public-base-url https://...` to `everflow daemon` and set `event_source: webhook` in the spec.

### 3. Write a spec

```markdown
---
goal: Replace internal/legacy with internal/v2 across services
provider: gitlab
project: acme/example
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

Poll mode (default — no public URL needed):

```bash
go build -o everflow .

./everflow daemon \
    --commit-author "Your Name" \
    --commit-email "you@example.com"
```

You should see:

```
provider registered  name=gitlab  auth=glab-oauth
everflow daemon started  listen=:8080  trigger_listen=127.0.0.1:8081 ...
```

The daemon now polls registered providers every 30s for events and listens for `everflow start` triggers on `127.0.0.1:8081`. No Anthropic API key needed — `claude -p` uses your interactive Claude Code login.

For webhook mode, add `--public-base-url https://...` and set `GITLAB_TOKEN`/`GITHUB_TOKEN` env vars with webhook permissions.

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
triggered run  run_id=...  mode=spec  provider=gitlab  project=acme/example
```

The daemon's `setup` step now: (a) calls the provider to fetch your authenticated user (Author), (b) creates `~/.everflow/runs/<runID>/` with a default Starlark filter, (c) in webhook mode only — generates a secret + registers a webhook. Then `discover` invokes the claude planner with the spec body to choose the first increment. Then `work` runs claude in a fresh worktree off `main`, commits + pushes (filtering binary build artefacts out of the staging — ADR-0032), opens the Draft MR, posts an initial status comment, and parks in `AwaitingMerge`. The poller picks up subsequent comments and CI events.

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

Any other comment, the Starlark filter classifies (bot/short/known-phrase → skip; substantive → invoke). On invoke, the runner addresses the comment, pushes a follow-up commit, and **auto-resolves the originating discussion thread**. If the runner produced no code change (verbal reply only), the thread is still marked resolved and the Run stays `AwaitingMerge` — no fatal pause for non-code answers (ADR-0034). CI failures route through the same filter.

### Known limitations

- **Single concurrency** in v1; the throttled-sequential model parallelises in v2.
- **No `everflow status` CLI** yet — use `/everflow status` on an MR comment, or `sqlite3 ~/.everflow/store.db "SELECT * FROM records"`.
- **No retry-replay**: `/everflow retry` clears the pause but the author has to re-trigger by event (re-comment, wait for CI rerun).
- **GitHub thread auto-resolve** is a no-op stub pending GraphQL `resolveReviewThread` work; GitLab resolves cleanly.

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
    --commit-author "Everflow Bot" \
    --commit-email "everflow@example.com"
Restart=on-failure
RestartSec=10s
# Optional — only for webhook mode:
# Environment=GITLAB_TOKEN=...

[Install]
WantedBy=multi-user.target
```

Laptop deployment is fully supported with poll mode — no tunnel needed. Switch to webhook mode (lower latency) by adding `--public-base-url` and a tunnel: Tailscale Funnel is the easiest (`tailscale funnel 8080`); ngrok / cloudflared also work. See [`DESIGN.md`](DESIGN.md#public-url-strategy).

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
| [`internal/filter/`](internal/filter/) | Starlark event filter with per-Run override + phrase learning (ADRs 0018, 0030) |
| [`internal/eventstream/`](internal/eventstream/) | In-process workflow.EventStreamer (cond.Wait based; ADR-0033) |
| [`internal/poller/`](internal/poller/) | Poll-mode event ingress (ADR-0031) |
| [`_v0/`](_v0/) | Archived scheduled-skill PoC, separate module |

## License

MIT
