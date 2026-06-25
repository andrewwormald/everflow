# ADR-0031: Polling as the primary event source; webhooks become opt-in

**Status**: Accepted (supersedes the "webhook-primary" framing implicit in ADR-0016)
**Date**: 2026-06-23

## Context

Everflow's headline promise (ADR-0014) is **amortising LLM cost**. The
state machine sleeps idle until something interesting happens, then
reacts. The original ingress design (ADR-0016) made *webhooks* the
channel — daemon registers a webhook on the provider, exposes a public
URL via a tunnel, verifies inbound HMAC, dispatches to `workflow.Callback`.

This conflated two distinct concerns:

- **"Don't poll the LLM"** — the brand promise. Polling claude/qwen
  every N seconds to ask "is there anything to do?" *would* defeat the
  cost story. We never wanted that.
- **"Don't poll the provider"** — what we accidentally also committed
  to. Polling `glab mr view` every 30 seconds is **free** (no LLM
  involved). It buys us simplicity at the cost of a few seconds of
  latency — fine for refactor sweeps that run over hours/days.

The first time we tried to set up a real spike against a production
monorepo, the webhook path's preflight cost became obvious:

- Install ngrok, mint authtoken
- Mint a personal-access-token with `admin:repo_hook` scope
- Expose laptop via a public tunnel
- Register a webhook on the production GitLab project (visible to
  everyone with read access)
- Manage HMAC secret lifecycle, rehydrate on daemon restart

For a laptop-spike, that's a lot of moving pieces for sub-second
latency we don't need.

## Decisions

### 1. Polling is the default; webhooks are opt-in

`AgentState.EventSource` is a new string field with values:

- `"poll"` — the daemon's poller goroutine queries the provider every
  ~30s for new comments + MR state changes on each Run's InFlight MRs.
  Synthesised `provider.Event` values flow through the same
  `workflow.Callback` path that webhooks use. **Default.**
- `"webhook"` — `setup()` registers a webhook on the provider; the
  daemon's HTTP server ingests. The original v1 design, still
  available for stable-URL VPS deployments.

Empty `EventSource` is treated as `"poll"`. This biases towards safety:
no accidental webhook registration on a shared production repo when
someone forgets to set the field.

### 2. The poller reuses the existing event + callback path

`internal/poller` ships a `Loop` with a 30-second ticker. Per tick:

- Iterate active Runs (RunState != finished) with EventSource=poll
- For each Run, for each MR in InFlight:
  - `provider.GetMRState(...)` — if state differs from `LastMRStates[iid]`,
    synthesise an `EventMRMerged` or `EventMRClosed` event
  - `provider.ListNotesSince(..., lastSeen)` — for each new note,
    synthesise an `EventNoteAdded` event
- Each synthesised event flows through the same `dispatcher` function
  that webhook delivery uses, which calls `workflow.Callback`, which
  invokes `resume()` exactly as if a webhook had arrived

`resume()` itself is unchanged. It updates `LastSeenNoteIDs[iid]` and
`LastMRStates[iid]` on the way through, keeping the watermarks
monotonic — the next poll won't re-fire already-handled events.

### 3. The provider interface gains two read-only methods

```go
GetMRState(ctx, projectID, mrIID) (state string, err error)
ListNotesSince(ctx, projectID, mrIID, sinceNoteID int64) ([]NotePoll, error)
```

GitLab: implemented via the same HTTP client (`GET .../merge_requests/:iid`
and `GET .../merge_requests/:iid/notes?sort=desc&...`).
GitHub: stub returning a clear error — v1 polling is GitLab-only.
GitHub spike users continue with webhooks (its HMAC story is cleaner
anyway).

### 4. GitLab auth gains a Bearer-token mode

`gitlab.Provider.Config` adds `AuthMode AuthMode` (default `AuthPAT`).
When `AuthBearer`, requests use `Authorization: Bearer <token>` instead
of `PRIVATE-TOKEN: <pat>`.

`gitlab.LoadGlabToken(host)` reads `~/Library/Application Support/glab-cli/config.yml`
(macOS) or `~/.config/glab-cli/config.yml` (Linux) and returns the
OAuth token there. `main.go`'s `buildProviders`:

- Try `GITLAB_TOKEN` env first → AuthPAT
- Fall back to `gitlab.LoadGlabToken("")` → AuthBearer
- If neither, no gitlab provider

This lets a personal-laptop spike work with **zero token setup**: as
long as `glab auth login` has been run interactively, everflow uses
that auth.

### 5. `--public-base-url` becomes optional

Previously required; now only used by webhook-mode Runs. Poll-mode
daemons start without it. If a webhook-mode Run is triggered while
`--public-base-url` is empty, `setup()` will fail at webhook-registration
time with a clear error mentioning the flag.

## Alternatives considered

- **Keep webhooks primary, add polling as fallback** — closer to ADR-0016's
  letter, but inverts the safer-default. New users hit the
  ngrok+PAT+admin:repo_hook wall first; we'd be opting them *out* of
  complexity rather than *in*.
- **Drop webhook code entirely** — too aggressive. Real production
  deployments on a VPS with a stable URL get a latency win from
  webhooks; we shouldn't take that away.
- **Use `glab` as a subprocess for polling** — would save the bearer-auth
  code, but adds 50-100ms exec overhead per call, makes responses
  harder to type, and breaks if `glab` updates change its CLI output
  format. HTTP API is the canonical surface.
- **One unified ingress** that abstracts over poll/webhook — overengineered.
  The two paths share `provider.Event` and `workflow.Callback`; that's
  enough abstraction.

## Consequences

- **Spike preflight collapses**. A spike against a production monorepo
  no longer needs ngrok, no PAT, no webhook registration on the target.
  Just `claude -p` works + `glab auth login` done + `everflow daemon`.
- **Latency**: poll-mode events arrive at the workflow within 30s of
  occurring, vs sub-second for webhooks. For refactor sweeps that run
  over hours/days, this is invisible. For real-time pair-programming
  use cases (out of scope today), webhooks remain the right call.
- **Provider API call cost**: a Run with one InFlight MR consumes
  2 API calls per 30s = 240/hour = ~6,000/day. GitLab's per-token
  rate limits are typically 600/min — well within budget. If a daemon
  hosts many simultaneous Runs (concurrency > 1 in a future commit),
  the poll interval will need to be tunable.
- **GitHub remains webhook-only in v1**. A future ADR can mirror the
  GitLab polling (the API exists: GET issues comments since N, PR
  state). Out of scope for the spike.
- **Daemon-restart resilience**: poll-mode is naturally resilient —
  watermarks live in `AgentState` (durable), so a restart-and-resume
  picks up exactly where it left off. Webhook mode still needs the
  secret-rehydration of ADR-0029.
- The original "Never polls" tagline gets a footnote: it's about LLM
  tokens, never about the provider API.
