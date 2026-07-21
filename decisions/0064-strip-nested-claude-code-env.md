# ADR-0064: Strip nested Claude Code session env vars from the daemon and its subprocesses

**Status**: Accepted
**Date**: 2026-07-21

## Context

`syntropy daemon` is routinely launched via `nohup syntropy daemon &` from
inside an active Claude Code session's Bash tool (this is how the daemon
was started throughout this repo's own dogfooding sessions). Claude Code
sets several environment variables on every process it spawns to identify
that process as a nested child of the invoking interactive session:
`CLAUDECODE`, `CLAUDE_CODE_SESSION_ID`, `CLAUDE_CODE_ENTRYPOINT`,
`CLAUDE_CODE_CHILD_SESSION`. A daemon launched this way inherits them, and
— since `internal/runner/claude.Runner.Run` builds its subprocess
environment via `cmd.Env = os.Environ()` with no filtering — every
`claude -p` invocation the daemon spawns for the rest of its lifetime
inherits them too.

This makes every daemon-spawned `claude -p` call look, from Claude Code's
perspective, like a nested child of whatever interactive session originally
started the daemon — a session that may still be actively running,
completely unaware of and unrelated to this specific background
invocation.

Found live: a Run's `discover` step (`planner: claude exec`) failed with
`exit status 1` on **9 consecutive attempts** (100% failure rate) while the
launching session was actively working, yet the exact same prompt/worktree
succeeded on manual, isolated reproduction **twice**. No other daemon-driven
concurrent activity was happening in the failure windows, ruling out
ordinary API rate-limiting from this daemon's own Runs. The one
environmental difference between the failing (daemon-spawned) and
succeeding (manually-run) invocations was this env-var inheritance —
consistent with a session-identity conflict specific to appearing as a
concurrent nested child of a busy parent session, not a transient API or
spec-content issue.

## Decision

Two layers, matching the two points where the daemon's process tree can
pick these variables up:

1. **`main.go`'s `cmdDaemon`** calls `unsetNestedClaudeCodeEnv()` once, at
   startup, before anything else — `os.Unsetenv` on each of the four vars
   for the daemon's own process. This is the primary fix: it means the
   daemon's env is clean for the rest of its lifetime regardless of how it
   was launched, and every subprocess it ever spawns (not just `claude`)
   inherits a clean environment.
2. **`internal/runner/claude.Runner.Run`** additionally calls
   `stripNestedClaudeCodeEnv(cmd.Env)` right before `exec.CommandContext`
   runs, filtering the same four variables (by exact-prefix match, e.g.
   `"CLAUDECODE="`) out of whichever env source was selected (`c.Env` if
   set, otherwise `os.Environ()`). This is defense-in-depth for the actual
   sensitive call site, independent of whether `cmdDaemon`'s startup fix
   ran — covers a `Runner` constructed and used outside `cmdDaemon` (e.g.
   directly in a test, or a future entry point), and guards against
   something else in the process re-setting these vars after startup.

Non-nesting-signal vars on the same `CLAUDE_CODE_`/`CLAUDE_` prefix family
(`CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING`, `CLAUDE_EFFORT`,
`CLAUDE_CODE_EXECPATH`) are deliberately left alone — they're general
configuration, not session-identity markers, and stripping them wouldn't
address the actual problem while needlessly changing daemon-spawned
`claude` behavior in ways an operator might not expect.

## Alternatives considered

- **Operational-only: always launch the daemon from a genuinely detached
  shell** (a real terminal, `launchd`/systemd unit, `setsid syntropy daemon
  &`), never from inside an active Claude Code session. Rejected as the
  sole fix — relies on remembering to do it a specific way every time, and
  doesn't help the (common, demonstrated-in-this-very-repo) workflow of
  starting the daemon directly from Claude Code's own Bash tool, which is
  a natural and convenient way to bootstrap it.
- **Do nothing in code; document the caveat.** Rejected — leaves a
  reproducible footgun for the next person (or agent) who starts the
  daemon this way, with a failure mode (silent, systematic `claude exec:
  exit status 1`) that's genuinely hard to diagnose without knowing to
  check the subprocess's inherited environment.
- **Only fix it in `claude.go` (skip the `cmdDaemon` startup unset).**
  Would still work for the `claude` runner specifically, but leaves the
  daemon's own environment polluted for every *other* subprocess it spawns
  (`git`, `gh`, `glab`) — none are known to be sensitive to these vars
  today, but there's no reason to leave that door open when unsetting once
  at startup is nearly free.

## Consequences

- A `syntropy daemon` process, however it was launched, now has a clean
  environment with respect to Claude Code session identity for its entire
  lifetime — this specific failure mode (spurious `claude exec: exit status
  1` correlating with the launching session being actively busy) should not
  recur.
- If Claude Code ever adds a new env var to its nested-session-identity
  family, both `nestedClaudeCodeEnvVars` (`main.go`) and
  `nestedClaudeCodeEnvPrefixes` (`internal/runner/claude/claude.go`) need
  updating together — they're intentionally duplicated (package boundary)
  rather than sharing a single source of truth, since `internal/runner/
  claude` doesn't import `main`. A future refactor could hoist this list
  into a small shared internal package if a third call site ever needs it.
- This was diagnosed via live reproduction rather than an authoritative
  statement from Claude Code's own documentation about how it treats
  nested/child sessions internally — the underlying mechanism (why a nested
  child conflicts with a busy parent specifically) is inferred from the
  observed 100%-fail-when-launched-nested vs 100%-succeed-when-run-directly
  pattern, not confirmed from source. If this fix doesn't fully resolve
  future recurrences, that inference is the first thing to revisit.
