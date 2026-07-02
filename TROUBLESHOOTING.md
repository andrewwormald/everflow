# everflow Troubleshooting Guide

This guide covers failure modes surfaced during the 2026-06 dogfood spikes and the recovery procedures shipped in the early-access hardening pass. It is written for technical users who are not familiar with everflow's internals, and is structured so that an AI coding assistant (e.g. a Claude Code session) can also use it to diagnose a stuck Run.

---

## How to inspect a Run's state

The first thing to do with any stuck or unexpected Run is to get a readable snapshot:

```
everflow status <runID>
```

This prints status, goal, units, token spend, events seen, pause reason, last error, and recent turns. If you don't have the runID handy, `everflow list` shows all Runs.

If the daemon is not running, you can query the sqlite store directly:

```bash
sqlite3 ~/.everflow/store.db \
  "SELECT run_id, status, updated_at FROM records WHERE workflow_name='refactor-sweep' ORDER BY id DESC LIMIT 10"
```

To inspect the full AgentState JSON for a specific Run:

```bash
sqlite3 ~/.everflow/store.db \
  "SELECT json(object) FROM records WHERE run_id='<runID>'" \
  | python3 -m json.tool | less
```

Fields of interest:
- `status` — maps to the AgentStatus int (1=Initiated, 2=Discovering, 3=Working, 4=AwaitingMerge, 5=Paused, 6=Completed, 7=Failed, 8=Cancelled, 9=AwaitingAbandonConfirm).
- `pause_reason` — non-empty when Paused; prefix `provider-auth:` indicates a token expiry (see below).
- `last_error` — the most recent unrecoverable error string.
- `total_tokens` — cumulative Anthropic API tokens used.

---

## How to read the daemon log

Run the daemon with `everflow daemon` and watch stdout. Key log lines:

| Message | Meaning |
|---------|---------|
| `msg="triggered run" run_id=... foreign_id=...` | Run was successfully triggered by `everflow start`. |
| `msg="webhook received" kind=... mr_iid=...` | A webhook event arrived from the provider. |
| `msg="poller: auth backoff set" failures=1 until=...` | Token expired; auth backoff started. |
| `msg="poller: auth backoff cleared after successful tick"` | Token was refreshed; normal polling resumed. |
| `msg="injected control command" run_id=... verb=...` | A CLI command (`abandon`, `resume`, etc.) was sent via the daemon. |
| `level=ERROR msg="..." err=...` | An error that may need attention. |

---

## Common failure modes

### 1. The runner claimed changes it didn't make ("hallucination")

**Symptom:** A `✓ Addressed` comment says "I've reduced the diff to 3 insertions, 15 deletions" but the PR shows 96 files changed, 60 deletions.

**Diagnosis:** Look at the comment body on the MR. Since the early-access hardening pass, every `✓ Addressed` and `🤖 Opened` comment includes an actual `Diff:` line from `git diff --shortstat`. Compare the runner's summary with the `Diff:` line in the same comment.

**Recovery:** If the runner summary disagrees with the actual diff, the change is still there — it just means the runner fibbed about its extent. The code change itself is real; the review should focus on the diff, not the runner's description. If the MR is genuinely unwanted, reply `/everflow skip` to close it and move on.

---

### 2. Log shows 401s from the provider every 30 seconds

**Symptom:** Daemon log is flooded with `poller: GetMRState auth failure; backing off` every 30 seconds. The Run appears stuck at `AwaitingMerge`.

**Root cause:** The provider token (GitHub OAuth or GitLab PAT) has expired or been revoked.

**What happens now:** After the first 401, the Run transitions to `Paused` with a `pause_reason` starting with `provider-auth:`. The daemon posts a comment on the in-flight MR explaining the situation, and the poller backs off exponentially (30s → 2m → 8m → 32m → 2h). The log noise stops immediately.

**Recovery:**

For GitHub OAuth (the common case when using `gh auth login`):
```bash
gh auth login
# follow prompts to refresh the token
```

For GitHub PAT (`GITHUB_TOKEN` env var): rotate the token in GitHub settings and restart the daemon with the new value.

For GitLab: similarly run `glab auth login` or rotate `GITLAB_TOKEN`.

Then restart the daemon:
```bash
# Ctrl-C the running daemon, then:
everflow daemon --store ~/.everflow/store.db
```

On the next poll tick (up to 2 hours if the backoff was deep), the Run will see a successful API call, dispatch `EventProviderAuthRestored`, and return to `AwaitingMerge` automatically. You can also accelerate this by running:
```bash
everflow resume <runID>
```

**Diagnosis via `everflow status`:**
```
Paused:   provider-auth: token expired or invalid — refresh via `gh auth login` ...
```

See [ADR-0038](decisions/0038-poller-auth-backoff-pause-marker.md) for the design.

---

### 3. PR was merged but no new MR was opened

**Symptom:** The PR merged successfully but the daemon never opened a follow-up MR. The Run appears stuck at `AwaitingMerge` or `Discovering`.

**Diagnosis:** Check `everflow status <runID>`:

- If `Completed`: the planner decided the spec is fully implemented. This is the normal happy path.
- If `Discovering`: the planner is running; wait a few minutes.
- If `Paused`: check `PauseReason`. The planner may have returned `DecisionAsk` because it needs input.
- If the queue or plan history shows no further units, the spec may be satisfied.

To give the planner more context, reply on the merged PR or use:
```bash
everflow resume <runID>  # if Paused
```

---

### 4. Run is Paused and I don't know why

**Symptom:** `everflow status <runID>` shows `Status: Paused` but the cause isn't obvious.

**Diagnosis:** The `Paused:` line in the status output contains the reason. Common reasons:

| Pause reason prefix | Cause | Recovery |
|---------------------|-------|---------|
| `provider-auth:` | Token expired | Refresh credentials + restart daemon, or `everflow resume` |
| `paused by /everflow pause` | Manual pause | Reply `/everflow resume` on the MR, or `everflow resume <runID>` |
| `runner error during` | Runner crashed or timed out | `everflow resume <runID>` (triggers retry on next event) |
| `planner asks:` | Planner needs input | Answer the question in a comment on the MR |
| `budget:` | Token or runtime budget exceeded | Raise budget or use `everflow resume` to accept the overage |

---

### 5. Daemon uses excessive CPU (≥ 300%)

**Symptom:** `top` or Activity Monitor shows `everflow daemon` using 300% CPU or more.

**Cause:** This was the memstreamer spin-loop bug fixed in [ADR-0033](decisions/0033-replace-memstreamer.md). If you see it on a recent build, it indicates a regression in the upstream luno/workflow library's event streamer.

**Recovery:** Restart the daemon. If the problem persists, file a bug with the daemon log and the output of `ps aux | grep everflow`.

---

### 6. PR contains a Makefile or other unwanted artefact

**Symptom:** The runner opened a PR that includes a `Makefile`, compiled binaries, or other files that shouldn't be there.

**Cause:** The runner overscoped its changes.

**Recovery:** Comment on the PR to redirect the runner:
```
/everflow prompt Do not create a Makefile or modify build tooling. Focus only on the specific files in scope.
```

Alternatively, close the PR with `/everflow skip <reason>` and let the planner try again.

---

## Recovery procedures

### When to use `everflow abandon`

Use `abandon` when the Run is stuck, the MR is unwanted, or you want to terminate a Run cleanly with in-flight MRs closed.

With the daemon running (two-tap confirmation):
```bash
everflow abandon <runID>        # first tap: confirmation prompt
everflow abandon <runID>        # second tap (within 12h): confirmed, MRs closed
```

With the daemon NOT running (immediate force-cancel):
```bash
everflow abandon --store ~/.everflow/store.db <runID>
```

This writes directly to sqlite, closes in-flight MRs via the provider credentials on disk, and removes worktrees. See [ADR-0037](decisions/0037-resume-cli-direct-store.md).

### When to use `everflow resume`

Use `resume` to restart a Paused, Failed, or Cancelled Run.

With the daemon running:
```bash
everflow resume <runID>
```

Without the daemon (direct store write):
```bash
everflow resume --store ~/.everflow/store.db <runID>
# Then restart the daemon to process the outbox event:
everflow daemon --store ~/.everflow/store.db
```

For a Failed Run, `resume` revives it to `Discovering` so the planner can pick up the next increment.

### When to edit sqlite directly (rare)

Direct sqlite editing is only needed if neither CLI command works and the workflow library's outbox mechanism isn't available (e.g. corrupted store). Before doing this, always try `everflow abandon` or `everflow resume` first. If you must:

```bash
sqlite3 ~/.everflow/store.db \
  "UPDATE records SET status=8, run_state=4 WHERE run_id='<runID>'"
```

Note: this does **not** insert an outbox event and will not reanimate the daemon. It only changes what `everflow status` reports. For a true revival, use `everflow resume`.

---

## What to attach when reporting a bug

To help diagnose an issue, collect:

1. **Daemon log** (last 500 lines from `everflow daemon` stdout)
2. **`everflow status <runID>` output**
3. **Recent records:**
   ```bash
   sqlite3 ~/.everflow/store.db \
     "SELECT run_id, status, run_state, updated_at FROM records WHERE workflow_name='refactor-sweep' ORDER BY id DESC LIMIT 5"
   ```
4. **AgentState JSON** (see above for the query)
5. **The spec file** that triggered the Run (if applicable)
6. **ADR references** for any failure mode matching a known design decision

File bugs at: https://github.com/andrewwormald/everflow/issues
