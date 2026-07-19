---
name: syntropy
description: Turn a large refactor or sweeping change into a chain of small, reviewable MRs/PRs, shepherded to merge one at a time by the syntropy daemon. Use when the user asks to sweep a change across many files or services, wants a long-running background loop that babysits a review/CI cycle, or mentions "syntropy" by name.
---

# syntropy

`syntropy` is a Go CLI + daemon that decomposes a large change into small
units, opens one draft MR/PR at a time, and only opens the next once the
current one merges. It runs independently of this session — once triggered,
it keeps working even after this conversation ends.

## When to use this skill

- The user wants a sweeping refactor applied across many files, packages, or
  services ("migrate every service off X", "rename Foo to Bar everywhere",
  "add a metric to every handler").
- The user wants something to "babysit" a PR/MR — keep it green overnight,
  respond to review comments, retry flaky CI — without you staying attached.
- The user explicitly mentions `syntropy`.

Don't use it for a single small, one-shot edit — just make the change
directly.

## Prerequisites

- `syntropy` binary on `$PATH` (confirm with `syntropy version`).
- `git` and `claude` on `$PATH`.
- A local clone of the target repo with an `origin` remote.
- Provider auth: `GITLAB_TOKEN` / `GITHUB_TOKEN` env var, or `glab auth
  login` / `gh auth login`.

## Basic flow

1. **Write a spec.** A markdown file with YAML frontmatter describing the
   goal, provider, project, and base repo. See `specs/README.md` in the
   syntropy repo for the anatomy, or ask the user for the details you need.
2. **Start the daemon** (if one isn't already running):
   ```bash
   syntropy daemon --commit-author "Name" --commit-email "you@example.com" &
   ```
3. **Trigger the run:**
   ```bash
   syntropy start --spec path/to/your.spec.md
   ```
4. **Check progress** any time with:
   ```bash
   syntropy status <run-id>
   syntropy list
   ```

The daemon opens the first MR/PR within a minute or two. Reviewers interact
with it entirely through MR/PR comments (`/syntropy status`, `/syntropy
pause`, `/syntropy skip`, `/syntropy retry`, `/syntropy prompt …`,
`/syntropy stop`) — you don't need to keep polling on the user's behalf
unless asked.

## Other commands

- `syntropy abandon <run-id>` — request abandonment (two-tap confirmation).
- `syntropy resume <run-id>` — resume a paused run.
- `syntropy phrases` — manage skip-phrase files.

Run `syntropy <command> -h` for full flag reference before constructing a
command — flags evolve independently of this skill file.
