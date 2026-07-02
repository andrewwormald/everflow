---
goal: "Early-access hardening: runner token accounting and Budget enforcement; `everflow status` + `everflow abandon` + `everflow resume` CLIs; cheap hallucination guard on runner Done; poll backoff on auth failure; troubleshooting guide"
provider: github
project: andrewwormald/everflow
runner: claude
base_branch: main
base_repo: /Users/andreww/dev/everflow
concurrency: 1
draft_mrs: true
status: ready
---

# Early-access readiness pass

The three dogfood spikes shipped so far ([ADR-0032](decisions/0032-staging-filters-binary-blobs.md), [ADR-0033](decisions/0033-replace-memstreamer.md), [ADR-0034](decisions/0034-comment-loop-and-paused-self-loop.md), [ADR-0035](decisions/0035-self-comment-echo-suppression.md)) proved the core loop works. This pass closes the gaps that block inviting a small circle of trusted external users. The scope is deliberately conservative — five items, each individually shippable, each producing one or two small MRs.

**Note:** an earlier revision of this spec included a sixth item (CI + CodeRabbit). That item was landed manually before triggering this Run, because a runner-driven MR touching `.github/workflows/*` requires the GitHub OAuth token to carry the `workflow` scope — which the daemon's `gh auth token` fallback does not have by default. CI is therefore already in place on `main` as of the trigger of this Run.

## Cross-cutting constraints (all MRs)

- **One concern per MR.** Each of the five items below expects 1-3 MRs. Do NOT bundle unrelated changes.
- **British English** in prose.
- **No customer-repo names in code or docs.** Use generic placeholders (`acme/example`).
- **Every MR must include tests** proportional to the change. Docs-only MRs excepted.
- **The end-to-end proof pattern** (see `TestSelfCommentLoop_EndToEnd`) is the model: for behavioural changes, at least one test must exercise the real code path, not a mocked shortcut.
- **Keep runner-token usage low.** Prefer deterministic checks (regex, git-diff parsing, sqlite queries) over LLM verification for any new logic.

## Suggested build order

The items can be tackled independently, but this order gates each on the infrastructure it depends on:

1. **Runner token accounting** (item 1) — foundational: budget enforcement, status CLI, and hallucination guard all benefit from real token numbers.
2. **`everflow status` CLI** (item 2) — reads existing state; no protocol changes; safe.
3. **`everflow abandon` + `everflow resume` CLIs** (item 3) — mutates state; touches the state machine.
4. **Hallucination guard on runner Done** (item 4) — needs token accounting so its own cost is visible; belongs with runner-adapter work.
5. **Poller auth-expiry backoff** (item 5) — the trickiest design; benefits from having status CLI to observe backoff behaviour.
6. **Troubleshooting guide** (item 7) — pure docs; last so it can reference all the new CLIs and behaviours.

The planner may reorder if it identifies a better dependency graph.

---

## Item 1 — Runner token accounting + `Budget` enforcement

### What

Populate `runner.Response.Tokens` from the actual `claude -p` output for the Claude adapter. Roll each turn's tokens into `Turn.Tokens` (already recorded). Add a running total to `AgentState` and enforce `AgentState.Budget.MaxTokens`.

### Why

Every `Turn` row in `AgentState.History` today shows `Tokens: 0` because the Claude adapter never populates `Response.Tokens`. This makes the existing `Budget` field unenforceable and leaves users with no visibility into runner spend. From the 2026-06-29 spike accounting, two Runs consumed ~200-500K runner tokens with no in-daemon signal; the leak was only visible in the Anthropic dashboard after the fact.

### Scope

- `internal/runner/claude/`: parse the `claude -p` output for token counts. Claude Code's `-p` mode reports token usage in a machine-readable form at the end of the response (verify the exact shape; adjust the decision-marker protocol if needed).
- `internal/runner/`: no protocol change beyond what already exists (`Response.Tokens int`).
- `internal/refactorsweep/types.go`: add a running total field to `AgentState`:
  - `TotalTokens int` — sum across all turns
- `internal/refactorsweep/workflow.go`: after each subagent turn (in `work` and `invokeForEvent`), accumulate `resp.Tokens` into `AgentState.TotalTokens`. If `AgentState.Budget.MaxTokens > 0` and `TotalTokens >= MaxTokens`, transition to `StatusPaused` with `PauseReason = fmt.Sprintf("budget exceeded: %d of %d tokens", TotalTokens, MaxTokens)`. Post a comment surfacing the pause.
- Ensure the pause is recoverable: `/everflow resume` from a budget-pause should clear the pause reason and continue (the user has raised their budget out-of-band or accepted the overage).

### Constraints

- **Do not** rebuild the runner protocol. Extend it minimally if the token parse needs a new marker.
- **Do not** silently absorb parse failures; if tokens can't be extracted, log a warning and continue with `resp.Tokens = 0` (regressing to current behaviour rather than crashing).

### Done when

- A Claude-runner turn's `Turn.Tokens` reflects the real Anthropic-reported input+output count.
- `AgentState.TotalTokens` accumulates across turns.
- Setting `budget.max_tokens` in a spec and exceeding it causes the Run to Pause with a clear reason and a comment on the in-flight MR.
- Tests: unit test on the parser (given a sample `claude -p` output, extracts the correct count), integration-style test on the workflow (Run with tight budget → hits pause).

---

## Item 2 — `everflow status <runID>` CLI

### What

Replace the current `status` subcommand stub in `main.go` with a real read-only CLI that prints a human-readable summary of a Run's state.

### Why

Today, inspecting a Run requires `sqlite3 ~/.everflow/store.db "SELECT ..."` plus knowing the JSON shape of `AgentState`. External users won't do that. Support cost drops significantly if they can run one command and get a legible snapshot.

### Scope

- Add `everflow status <runID>` (accept full or short prefix).
- Read the record from the sqlite store, unmarshal `AgentState`, print a structured summary. Suggested output shape:

  ```
  Run <shortRunID> — <foreign_id>
  Workflow status: <AgentStatus> (RunState: <run_state>)
  Mode: <spec|sweep>   Provider: <name>   Project: <projectID>

  Current unit: <unitID>
  In-flight:     <count> MR(s):
                   #<iid> <url>  (branch: <branch>)
  Completed:     <count>
  Blacklisted:   <count>  <last-reason>

  Events seen:   <n>
  Events skipped (filter): <n>
  Subagent calls: <n>
  Total tokens:  <n>  (of <budget.max_tokens or "unlimited">)

  Pause reason:  <if paused>
  Last error:    <if any>

  Recent turns (last 5):
    [work]          increment-2   tokens=1240   "<summary excerpt>"
    [address_comment] increment-2 tokens=890    "<summary excerpt>"
    ...
  ```

- If runID prefix matches multiple records, error with the matching runIDs listed.
- If runID doesn't match, error with "no such Run" and a hint to use `everflow list` (if that exists yet; otherwise the sqlite query).

### Constraints

- Read-only. No sqlite mutations, no daemon interaction, no HTTP calls.
- Does not require the daemon to be running.

### Done when

- `./everflow status <runID>` prints the summary above for any Run in the sqlite store.
- Tests: unit test that seeds a Record into a temp store, runs the command, verifies the printed output contains expected fields.

---

## Item 3 — `everflow abandon <runID>` + `everflow resume <runID>` CLIs

### What

Two mutating CLI subcommands:

- `everflow abandon <runID>` — force-terminates a Run that's stuck or unwanted. Sets `Status = StatusCancelled`, closes any in-flight MRs (best-effort via the provider), removes worktrees. Works even for Runs that died in `setup` before an MR existed.
- `everflow resume <runID>` — takes a `Status = StatusCancelled` or `Status = StatusFailed` Run and revives it back to `Status = StatusDiscovering` so the state machine can pick up the next planner turn.

### Why

Today the only recovery path for a stuck Run is `sqlite3 UPDATE records SET status = ...` (as we did on 2026-06-29 with Run `cc2383f8`) — which doesn't actually reanimate the Run because the workflow library consumes from the outbox, not by polling records. External users cannot recover from this state without significant familiarity with the workflow library's internals.

### Scope

- `everflow abandon <runID>`:
  - Read the Record; refuse if already terminal-Cancelled or terminal-Completed (idempotent).
  - Set `Status = StatusCancelled`, `RunState = RunStateCancelled`, `LastError = "abandoned via CLI"`.
  - For each MR in `InFlight`: call `provider.CloseMR` best-effort; don't fail the abandon if a close call errors.
  - Remove per-Run worktrees (best-effort via `git.RemoveWorktree`).
  - Persist via the workflow store (bumps `Meta.Version` correctly).

- `everflow resume <runID>`:
  - Read the Record; refuse if not terminal (`Status` must be `Cancelled` or `Failed`).
  - Set `Status = StatusDiscovering`, `RunState = RunStateRunning`, clear `LastError` and `PauseReason`.
  - **Crucially**: inject a synthesised event into the workflow's outbox so the `Discovering` consumer actually fires on the next tick. This is the piece that manual sqlite editing gets wrong. Look at the workflow library's `Trigger`/`Callback` internals to find the correct API — likely `workflow.MakeOutboxEventData` + direct outbox insertion, or a workflow.Trigger-with-same-foreign-id semantic.
  - **Design note**: if you can't cleanly inject an outbox event through a supported API, propose an alternative in the ADR (e.g. a dedicated `Resuming` status with an `AddStep` that transitions back to `Discovering`, triggered via a new HTTP endpoint on the daemon). Whatever's chosen must actually work end-to-end.

### Constraints

- Both commands work against the sqlite store on disk. The `abandon` command does NOT require the daemon to be running (it's a rescue tool); `resume` MAY require the daemon to pick up the new outbox event on its next consumer poll.
- Neither command touches state on the provider beyond what's necessary (close MRs on abandon; nothing on resume).

### Done when

- A Run in status Failed or Cancelled can be revived to Discovering via `everflow resume`, and on daemon restart / next tick the state machine picks up where the resume left it.
- A stuck Run in any non-terminal status can be forcibly Cancelled via `everflow abandon`, with its in-flight MRs closed.
- Integration test: seed a Failed Run, run `resume`, restart the workflow, assert the Run advances (planner is invoked).
- An ADR captures the design choice for how `resume` injects the event (this is a load-bearing decision).

---

## Item 4 — Cheap hallucination guard on runner Done

### What

After a runner turn returns `DecisionDone`, verify the runner's `Summary` doesn't contradict the actual `git diff` in the worktree. If mismatch is detected, mark the turn as suspicious in the address_comment reply so the reviewer sees the discrepancy immediately.

### Why

The 2026-06-29 spike showed the runner claiming *"The MR is now down to 3 insertions, 15 deletions"* when the actual PR was unchanged at 96/60. The `!dirty` and `ErrNoChanges` paths (ADRs 0032/0034) catch some hallucinations by requiring actual file changes, but they don't catch a runner that produces SOME changes while lying about their extent or nature.

### Scope

Two approaches, ordered by increasing invasiveness — pick whichever produces a usable guard within the token constraint:

**Approach A — Diff-append (no protocol change):**
- After a successful `git.Commit` in either `work` or `invokeForEvent`, capture `git diff --shortstat main..HEAD` (or against `TargetBranch`).
- Append the actual shortstat to the address_comment reply body under a `Diff:` line. Example:

  ```
  ✓ Addressed (address_comment): <runner summary>

  Diff: 3 files changed, 12 insertions(+), 4 deletions(-)
  ```

- No hallucination detection; but the reviewer sees both the runner's claim and the actual number, making a lie visible in the same comment.

**Approach B — Structured file-list verification (protocol extension):**
- Extend the `<everflow-decision>` marker protocol to accept an optional `files-changed:` block listing the file paths the runner claims to have modified.
- The Claude runner adapter parses this into `Response.FilesChanged []string`.
- After Commit, compute `git diff --name-only main..HEAD`. If the runner's claimed set doesn't equal the actual set (either extra claims OR missing files), post a comment:

  ```
  ⚠️ Runner claimed to modify [a.go, b.go] but diff shows [a.go, c.go].
  Manual review required.
  ```

- Optionally: also route this through the state machine as a soft-Pause if the mismatch is severe.

**Prefer Approach A initially** — zero protocol change, keeps runner-token cost identical (the shortstat is a `git` call, not a `claude -p` call), and makes hallucinations immediately visible without adding complexity.

### Constraints

- **No LLM verification.** The check must be deterministic and cost zero runner tokens.
- Post the diff shortstat only on comments where the runner claimed a code change (`DecisionDone` + `dirty`). Don't spam it on `!dirty` info-only replies.

### Done when

- Every `✓ Addressed` comment from `invokeForEvent` and every initial `🤖 Opened` comment from `work` includes an actual `git diff --shortstat` line reflecting what was actually pushed to the branch.
- Tests: unit test that a runner Turn producing files X, Y creates an address_comment whose body contains `git diff --shortstat` output; end-to-end test in the style of `TestSelfCommentLoop_EndToEnd` that runs the real path and asserts the comment body includes both the summary and the actual diff.

---

## Item 5 — Poller auth-expiry backoff

### Design context (read before implementing)

The workflow is fundamentally async: when the poller sees a 401 from the provider, we can't cleanly synchronously pause the Run — the poll runs on its own goroutine, the Run's state lives in sqlite, and the state machine consumes events on a separate consumer loop.

Today (as of 2026-06-29): a token expiry causes the poller to log 401 warnings every 30 seconds forever. The Run continues to appear "alive" in the state machine (`AwaitingMerge`) but is functionally deaf — no new comments or merges reach `resume()`.

The user's suggested shape: use workflow status transitions + timeouts (~5min) to encode the backoff, rather than tracking retry counters in the poller alone. That gives us observability (the Run's status reflects that it's degraded) and recovery (a successful poll clears the backoff and returns to normal cadence).

### What

When the poller receives a 401 from the provider for a Run, the Run should:
1. Transition (via callback) to a new status `AwaitingProviderAuth`, or reuse `Paused` with a machine-parseable `PauseReason` prefix like `provider-auth:`.
2. Post a comment to the MR (or to the Run's audit log) noting the auth failure and asking the user to refresh their token.
3. The poller polls a Run in the auth-backoff status at a slower cadence — 5-minute intervals instead of 30s — to reduce log noise + API waste while the user refreshes.
4. On the next successful poll (any provider call that returns 2xx), transition the Run back to its previous status and resume the normal 30s cadence.

### Scope

- New AgentStatus **`AwaitingProviderAuth`** (or reuse `Paused` with a marker; choose based on state-graph impact — ADR the decision either way).
- Poller: detect 401 (via a provider-agnostic error type; the provider adapters wrap their errors), transition the Run once (idempotent), set `AgentState.NextPollAt` to `now + 5m`.
- Poller loop: honour `NextPollAt` if set; skip Runs whose time hasn't arrived; on skipped Runs, don't log.
- On any successful poll, clear the auth-backoff state and return to normal cadence.
- Callbacks: register callback for the new status so `/everflow resume` (item 3) can also unstick it.

### Alternative under consideration

If the workflow-library timeout API (`b.AddTimeout(...)`) is the natural fit for the 5-minute backoff (rather than inline polling logic), use it. The user suggested this shape.

### Constraints

- **No unbounded retry**. Every backoff tick should be ~5 minutes minimum. Don't hammer the provider.
- The transition must be idempotent — a Run already in `AwaitingProviderAuth` shouldn't re-transition on every 401.

### Done when

- A Run whose provider token expires stops spamming logs within 30 seconds and enters `AwaitingProviderAuth` (or the chosen equivalent) with a clear reason.
- After the user refreshes the token (either by re-running `gh auth login` for OAuth or by rotating an env-var PAT and restarting the daemon), the next 5-min poll succeeds and the Run transitions back to its prior status.
- Test: fake provider returns 401 on `GetMRState`; assert the Run transitions to `AwaitingProviderAuth` within one poll cycle; assert subsequent polls are spaced 5min apart until the fake returns success.
- ADR records the design choice: dedicated status vs Paused-with-marker vs workflow-timeout.

---

## Item 7 — Troubleshooting guide

### What

A markdown file — `TROUBLESHOOTING.md` at the repo root — written to be readable by both humans AND external contributors' AI agents (e.g. Claude Code sessions helping someone debug a stuck Run). Covers the failure modes the three dogfood spikes surfaced plus the recovery procedures now available via the CLIs shipped by items 2 and 3.

### Why

The README covers the happy path. When something goes wrong, users today have to trace through the daemon log, query sqlite, and understand the state machine. This document short-circuits that.

### Scope

Structure (this is a suggestion; the runner may reorganise if a better shape emerges):

- **How to inspect a Run's state** — `everflow status <runID>` (item 2), and the sqlite queries it wraps for anyone who wants to peek deeper.
- **How to read the daemon log** — key log lines and what they mean (`msg="webhook received"`, `msg="triggered run"`, `msg="poller: ..."`, common ERROR patterns).
- **Common failure modes and their signatures**, one section each:
  - "The runner posted a comment claiming X but the diff shows Y" (hallucination — see item 4)
  - "Log shows 401s from the provider every 30s forever" (auth expiry — see item 5)
  - "PR merged but no new MR opened" (planner returned NoChange, or spec is satisfied — check `everflow status`)
  - "Run is Paused and I don't know why" (`everflow status` → PauseReason; or query `AgentState.pause_reason`)
  - "Daemon uses 380% CPU" (memstreamer bug — should not happen after ADR-0033; if it does, upstream regression)
  - "PR has a Makefile / other unwanted artefact" (runner overscoped — comment on the PR to redirect; see the dogfood spike memory)
- **Recovery procedures** — how to use `everflow abandon`, `everflow resume`, when to prefer each, when to fall back to editing sqlite (rare).
- **What to attach when reporting a bug** — daemon log, `everflow status <runID>` output, `sqlite3 ... "SELECT ... FROM records ..."`, ADR context.

### Constraints

- **Written in second-person present tense** ("If your Run is stuck at Paused, run `everflow status <runID>` to see the reason.")
- **British English** consistent with the rest of the repo.
- **Assume the reader is technical** but not familiar with everflow internals.
- **Cross-link generously to ADRs** where a failure mode has a design-decision reference.

### Done when

- `TROUBLESHOOTING.md` exists at the repo root.
- All six failure modes above are documented with symptom → diagnosis → recovery.
- The README's "Known limitations" section links to it.
- Every CLI shipped by items 2 and 3 is documented in the recovery-procedures section.

---

## Final done-when for this whole spec

- All five items in this spec shipped as merged PRs on `andrewwormald/everflow` (CI + CodeRabbit was landed manually prior to trigger, out of scope of this Run).
- CI stays green on `main` throughout the Run.
- Running `everflow status <runID>` on any existing Run produces a legible summary.
- Running `everflow abandon <runID>` + `everflow resume <runID>` on a test Run demonstrably works end-to-end.
- A user whose OAuth token expires mid-Run sees the Run transition to `AwaitingProviderAuth` (or equivalent) within 30 seconds, not silently spam 401s.
- `TROUBLESHOOTING.md` exists and covers the failure modes.
- Token counts on `Turn.Tokens` are non-zero for Claude-runner turns.
