# ADR-0063: GitLab provider resolves its token fresh per request, not once at startup

**Status**: Accepted
**Date**: 2026-07-21

## Context

`buildProviders` (`main.go`) falls back to `gitlab.LoadGlabToken("")` —
reading the OAuth access token out of `glab`'s own config file — when
`GITLAB_TOKEN` isn't set (ADR-0031's personal-laptop-spike rationale).
It called this once, at daemon startup, and passed the resulting
string into `gitlab.Config.Token`, which `Provider` then cached for its
entire lifetime.

`glab`'s OAuth access token is short-lived; `glab` itself transparently
refreshes it (via its refresh token) on every `glab` invocation,
rewriting its config file with a new access token. Our daemon's cached
copy has no way to notice this — it keeps sending the token that was
valid at daemon-start, and once that token's own natural TTL passes,
every GitLab API call 401s, indefinitely, regardless of whether `glab
auth status` shows the user as currently, freshly authenticated (it
reflects `glab`'s own refreshed token, not the daemon's stale copy).

Found live: a Run's `setup` step failed continuously with `gitlab API
/api/v4/user: status=401 invalid_token: Token is expired` — hours into
a long-running daemon process, while the operator's own `glab auth
status` reported a valid login. Compounded by the retry-storm bug
fixed in ADR-0062 (no backoff meant this got hammered every ~1s), but
the root cause is this ADR's concern, not that one's: a token that a
retry could never fix no matter the backoff, because the assumption
that a start-of-process snapshot stays valid forever is simply wrong
for this credential type.

## Decision

`gitlab.Provider` gains an optional `tokenSource func() (string,
error)` (set via `Config.TokenSource`), which — when set — takes
precedence over the static `Config.Token` and is called fresh on
**every** request in `do()`, rather than once at construction.
`buildProviders`'s glab-fallback branch now passes `TokenSource: func()
(string, error) { return gitlab.LoadGlabToken("") }` instead of reading
the token once and caching the string; the `LoadGlabToken` call that
remains inline is purely a fail-fast existence check ("is there a
token configured at all right now"), not a value that gets used.

`Config.Token` (the `GITLAB_TOKEN` env-var / PAT path) is untouched —
personal/project/group access tokens don't rotate on their own the way
an OAuth access token does, so a static string remains correct and
simpler for that path.

## Alternatives considered

- **Detect 401s and re-fetch the token reactively, retrying the failed
  request once.** More surgical (only re-reads the config file when a
  request actually fails), but adds retry-with-reauth logic to `do()`
  that has to be careful not to loop forever on a token that's
  genuinely gone stale for a different reason (revoked scope, deleted
  app). Rejected for now: `LoadGlabToken` is a cheap local file read —
  paying that cost on every request is not a meaningful performance
  concern at this daemon's request volume, and it's simpler to reason
  about ("always fresh") than "fresh only after first observed
  failure."
- **Cache the token with a TTL and refresh proactively before it
  expires.** Requires knowing the token's actual expiry (not exposed by
  `glab`'s config file, which just stores the current access token, not
  its issued-at/expires-at) — would need to either parse the JWT itself
  (brittle, couples us to `glab`'s current token format) or guess a TTL.
  Rejected: re-reading the file per-request sidesteps needing to know
  the expiry at all.
- **Do nothing here; rely on ADR-0062's circuit breaker to contain the
  damage.** Rejected — that ADR bounds the blast radius of *any*
  persistently-failing step, but doesn't fix the fact that this
  specific failure is permanent (until a daemon restart) and 100%
  avoidable, not transient. Papering over a fixable root cause with a
  circuit breaker meant for genuinely unpredictable failures isn't the
  right target for it.

## Consequences

- The GitLab provider's OAuth path (`AuthMode: AuthBearer`, glab
  fallback) no longer requires a daemon restart to pick up a token
  `glab` has refreshed in the background — this failure mode cannot
  recur for that path.
- One extra small file read (`LoadGlabToken`) per GitLab API call. Not
  measured, but expected to be negligible relative to the network round
  trip that follows it.
- The `GITHUB_TOKEN`/`gh auth login` path (`internal/provider/github`)
  has the same shape (`buildProviders` reads `github.LoadGhToken` once
  at startup) and is very likely subject to the identical staleness
  bug, but is out of scope for this ADR — tracked separately.
- `gitlab.Config` now has two ways to supply credentials
  (`Token`/`TokenSource`) that must be kept mutually exclusive in
  intent (`TokenSource` wins if both are set) — any future caller
  adding a third credential path should follow the same
  resolve-fresh-per-request shape rather than reintroducing a cached
  string.
