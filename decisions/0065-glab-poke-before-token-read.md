# ADR-0065: Poke `glab` to force its own token refresh before reading its config

**Status**: Accepted
**Date**: 2026-07-21

## Context

ADR-0063 fixed `gitlab.Provider` caching a one-time snapshot of `glab`'s
OAuth access token, replacing it with `TokenSource: func() (string, error)
{ return gitlab.LoadGlabToken("") }` — reading the token fresh from `glab`'s
config file on every request instead of once at daemon startup.

That fix was necessary but turned out not to be sufficient. Found live,
after ADR-0063 had already shipped and been rebuilt into the running
daemon: a Run's poller still hit `gitlab API ...: status=401
{"error":"invalid_token","error_description":"Token is expired..."}` on
three separate occasions over ~11 minutes, correctly tripping the
pre-existing provider-auth pause (ADR-0038) each time — while `glab auth
status`, run manually moments later, reported a healthy login.

The reason: `glab`'s access token is short-lived, and `glab` only refreshes
it **lazily**, as a side effect of actually being invoked (it checks
expiry, exchanges its stored refresh token for a new access token via
GitLab's OAuth endpoint, and rewrites its config file — confirmed by
directly testing that `glab api user` and `glab auth status` both leave a
freshly-refreshed token on disk). `LoadGlabToken` alone just reads whatever
happens to be in that file; if nothing has invoked `glab` recently enough,
the on-disk token can itself be genuinely expired, and reading it "fresh"
every request doesn't help — there's nothing to refresh *to*.

## Decision

Add `gitlab.RefreshGlabToken(host string) (string, error)`
(`internal/provider/gitlab/glabauth.go`): before calling `LoadGlabToken`,
it runs `glab api user` (bounded by a 10s timeout) purely to trigger
`glab`'s own internal refresh-if-needed logic as a side effect — the
command's own output/result is discarded entirely. `buildProviders`
(`main.go`) now wires `TokenSource: func() (string, error) { return
gitlab.RefreshGlabToken("") }` instead of calling `LoadGlabToken` directly.

The poke is best-effort and never blocks the read: if `glab` isn't on
`$PATH`, the network is unreachable, or the poke command itself fails for
any reason, `RefreshGlabToken` still proceeds to `LoadGlabToken` and
returns whatever it finds (or its error) — exactly as if no poke had been
attempted. A failed poke isn't proof the stored token is unusable (it
might already have been fresh); a successful poke doesn't guarantee
`LoadGlabToken` succeeds either (the config could still be missing the
requested host). This mirrors `LoadGlabToken`'s own existing "fall through,
don't hard-fail" philosophy (`ErrGlabNotConfigured` lets callers fall back
to an env-var PAT).

`glab api user` was chosen as the poke command over a lighter option like
`glab auth status` because it was directly, empirically confirmed (by
manual testing during this investigation) to trigger a real refresh — its
choice isn't drawn from `glab`'s documentation, since this behavior isn't
part of `glab`'s public contract (see Consequences).

## Alternatives considered

- **Implement OAuth refresh-token exchange directly**, reading `glab`'s
  refresh token and calling GitLab's `/oauth/token` endpoint ourselves
  in-process. Rejected: requires knowing `glab`'s registered OAuth client
  ID and reverse-engineering enough of its config format to extract the
  refresh token reliably; introduces a second writer racing `glab` itself
  on the same config file (both could refresh and rewrite it around the
  same time); duplicates logic `glab` already implements and maintains,
  for the sole benefit of avoiding one subprocess spawn per request.
- **Poke on a background timer instead of per-request** (e.g. every few
  minutes, keeping the token perpetually warm). Rejected for this
  increment: adds a goroutine/ticker to manage across daemon
  start/stop, and per-request poking is simpler to reason about — revisit
  if the subprocess overhead per GitLab API call becomes a measured
  problem, not a theoretical one.
- **Poke only reactively, after a 401**, retrying the original request
  once. Would avoid the overhead on the (usual) case where the on-disk
  token was already fine. Rejected for this increment in favor of the
  simpler "poke, then read, every time" shape; the token freshness bug
  this ADR fixes was severe enough (silent hours-long provider-auth
  pauses) that unconditional correctness was judged worth more than
  shaving the common-case latency.

## Consequences

- Every GitLab API call the daemon makes now costs one extra `glab api
  user` subprocess spawn plus a real network round trip (bounded to 10s) —
  meaningfully more latency per call than a pure local file read. Not
  measured against production GitLab call volume; revisit toward a
  background-timer or reactive-only poke (see Alternatives) if this
  becomes a real bottleneck.
- This fix depends on `glab`'s *undocumented* internal behavior — that
  invoking it triggers a refresh-if-needed check before making its own API
  call — rather than a stated public contract. If a future `glab` release
  changes when/how it decides to refresh, this could silently stop
  helping; the diagnostic pattern to watch for is the same one that led to
  this ADR (401s on daemon-driven calls while `glab auth status`, run
  manually, reports healthy).
- The GitHub-side equivalent (`buildProviders`' `gh auth login` fallback,
  tracked separately alongside ADR-0063's Consequences) is very likely
  subject to the same underlying "reads a config file that only gets
  refreshed by invoking the CLI" pattern — the same poke-then-read shape
  should be checked there too when that follow-up lands.
