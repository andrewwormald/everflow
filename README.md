<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="logo/mark-dark.gif">
    <img src="logo/mark.gif" alt="Everflow" width="220" />
  </picture>

  <h1>everflow</h1>

  <p><strong>Turn one large change into a chain of small, individually-reviewable MRs — and shepherd each one to merge before opening the next.</strong></p>
</div>

---

## What it is

You hand everflow a spec — "migrate `internal/legacy` to `internal/v2` across services", "rename `Foo` to `Bar` everywhere", "add a metric to every HTTP handler". A small Go daemon decomposes it into one increment at a time, opens a single Draft MR, waits for a human to actually merge it, and only then opens the next. Concurrency is configurable; default is one MR in flight.

```
     spec.md
        │
        ▼
     ┌─────┐   ┌─────┐   ┌─────┐   ┌─────┐
     │ MR1 │──►│ MR2 │──►│ MR3 │──►│ MR4 │──►  ... until done
     └──┬──┘   └──┬──┘   └──┬──┘   └──┬──┘
        │        │         │         │
        ▼        ▼         ▼         ▼
     review+  review+   review+   review+
     merge    merge     merge     merge
       (one at a time — gated on real review bandwidth)
```

Each MR is small — typically tens of lines, one logical change, scoped to a single unit. The next link in the chain doesn't exist yet when you're reviewing the current one.

## Why it helps

The two existing options for sweeping refactors are bad:

**One giant PR.** 47 services in one branch, `+1,247 / -863`. The reviewer needs an afternoon. They don't have one. The PR sits, conflicts pile up, eventually someone rubber-stamps it to clear the queue. Quality collapses; the diff is too big to engage with.

**You crank one at a time.** Open MR, wait for review, merge, manually do the next, repeat 47 times. You're the bottleneck between every link. You can't go to a meeting; you can't go on holiday; your day is spent waiting and ticking. The discipline is real but the cost is your week.

Everflow puts a daemon in the loop instead of you. The chain self-propels: you act only at the natural human checkpoint — *is this MR good?* — and the daemon handles everything else (opening, pushing, status comments, addressing review comments, retrying flaky CI, picking the next unit, opening the next MR).

**You get the benefits of disciplined small-MR practice without the discipline being your problem.** Each MR is small because everflow split the work; each MR ships fast because reviewers can actually engage with it; the sweep completes because there's never a stalled mega-PR clogging the queue.

## Inside each MR

```
   ┌──────────────────────────────────────────────────┐
   │  MR opens (Draft, small scope, one unit)         │
   │                                                  │
   │  Human reviews — 30-60 seconds                   │
   │     │                                            │
   │     ├── approve + merge ─────► next MR opens     │
   │     │                                            │
   │     ├── "/everflow skip" ────► unit blacklisted, │
   │     │                          next MR opens     │
   │     │                                            │
   │     └── request change ──────► runner pushes     │
   │                                a fix,            │
   │                                resolves the      │
   │                                thread,           │
   │                                back to review    │
   └──────────────────────────────────────────────────┘
```

Comments are everflow's only communication channel. Reply with `/everflow status`, `/everflow pause`, `/everflow skip`, `/everflow retry`, `/everflow prompt …`, or `/everflow stop`. Bot noise (CI status, formatter comments) is skipped deterministically by a Starlark filter, so the LLM only fires when a comment or a CI failure actually needs reasoning.

## How it works (briefly)

- **Durable state machine.** Built on [luno/workflow](https://github.com/luno/workflow); sqlite-backed RecordStore. Survives daemon restart, can sleep idle for days between events at zero LLM cost.
- **Event-driven.** Polls the provider every 30 seconds by default (zero token cost; ADR-0031). Webhook mode available for sub-second latency on hosts with a stable public URL.
- **Per-unit git worktree.** Each MR's runner works in `~/.everflow/runs/<runID>/worktrees/<unitID>` — no contamination of your main checkout.
- **Auto-resolve on push.** When the runner addresses a reviewer comment and lands the fix, the discussion thread is marked resolved automatically on both GitLab and GitHub (ADR-0034). The reviewer sees their comment close itself.
- **Pluggable runner.** Claude is the only shipping adapter today; Qwen / OpenHands / a local script all fit the `runner.Runner` interface.

Full architecture: [`DESIGN.md`](DESIGN.md). Every meaningful design choice has an ADR in [`decisions/`](decisions/).

## Quick start

You need: Go 1.26+, `git` and `claude` on `$PATH`, a clone of the target repo with an `origin` remote, and provider auth — either an env var (`GITLAB_TOKEN` / `GITHUB_TOKEN`) or an interactive CLI login (`glab auth login` for GitLab, `gh auth login` for GitHub). If both are configured, the env var wins.

```bash
# Write a spec.
cat > ~/everflow-specs/migrate.spec.md <<'YAML'
---
goal: Replace internal/legacy with internal/v2 across services
provider: gitlab
project: acme/example
runner: claude
base_branch: main
base_repo: /home/you/dev/your-repo
concurrency: 1
draft_mrs: true
status: ready
---
# Migration plan

For each service still importing `internal/legacy`, switch to
`internal/v2`. Preserve public function signatures.
YAML

# Build + start the daemon (poll mode; no public URL needed).
go build -o everflow .
./everflow daemon --commit-author "Your Name" --commit-email "you@example.com" &

# Trigger.
./everflow start --spec ~/everflow-specs/migrate.spec.md
```

The first MR appears on the target repo within a minute or two. Review it, merge it, and the next opens automatically.

## Repository layout

| Path | Purpose |
|---|---|
| [`README.md`](README.md) | This file |
| [`DESIGN.md`](DESIGN.md) | Full architecture and roadmap |
| [`AGENTS.md`](AGENTS.md) | Working rules for AI contributors |
| [`decisions/`](decisions/) | Architecture Decision Records — every meaningful choice |
| [`logo/`](logo/) | Brand assets |
| `main.go` | CLI entrypoint: `daemon`, `start`, `status`, `version`, `runners` |
| [`internal/refactorsweep/`](internal/refactorsweep/) | State machine + step bodies + control verbs |
| [`internal/provider/{gitlab,github}/`](internal/provider/) | Platform adapters |
| [`internal/runner/claude/`](internal/runner/claude/) | Claude shell-out runner with decision-marker parsing |
| [`internal/git/`](internal/git/) | `git` CLI wrapper with binary-blob filter |
| [`internal/store/`](internal/store/) | Sqlite-backed `workflow.RecordStore` + `TimeoutStore` |
| [`internal/spec/`](internal/spec/) | Spec markdown parser (frontmatter + body) |
| [`internal/filter/`](internal/filter/) | Starlark event filter with per-Run override + phrase learning |
| [`internal/eventstream/`](internal/eventstream/) | In-process `workflow.EventStreamer` (cond.Wait based) |
| [`internal/poller/`](internal/poller/) | Poll-mode event ingress |
| [`internal/webhook/`](internal/webhook/) | HTTP webhook ingress (opt-in) |
| [`_v0/`](_v0/) | Archived scheduled-skill PoC, separate module |

## License

MIT
