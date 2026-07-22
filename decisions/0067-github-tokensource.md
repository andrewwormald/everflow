# ADR-0067: GitHub provider resolves its token fresh per request, not once at startup

**Status**: Accepted
**Date**: 2026-07-22

## Context

ADR-0063 fixed the GitLab provider caching a one-time snapshot of `glab`'s
OAuth token at daemon startup, and ADR-0065 found that reading fresh
wasn't even sufficient there — `glab` only refreshes lazily, as a side
effect of being invoked, so its own config file needed to be poked
(`glab api user`) before every read.

`buildProviders` (`main.go`) had the exact same startup-snapshot bug on
the GitHub side: `github.LoadGhToken("")` was called once, and its result
passed as a static `Token` into `github.New(...)`, cached in `Provider`
for the daemon's lifetime.

The GitHub side turns out to need less work than the GitLab poke-fix,
because `LoadGhToken` (`internal/provider/github/ghauth.go`) already
shells out to `gh auth token --hostname <host>` on **every** call — unlike
`gitlab.LoadGlabToken`, which does a plain local file read with no live
`gh`-equivalent CLI invocation involved. `gh auth token` is a real,
live invocation of the `gh` binary, which handles its own refresh
internally the same way `glab api user` does for GitLab (confirmed
directly for GitLab in ADR-0065; not independently re-confirmed for `gh`
in this ADR, but the mechanism — a CLI shelling out to itself checks/
refreshes before returning — is the same shape and there's no evidence of
a plain-file-read path for gh the way glab has). So `LoadGhToken` already
*is* the "poke + read" combined into one call; the only bug on this side
was `buildProviders` calling it once and caching the result, rather than
via `TokenSource`.

## Decision

`github.Provider` gains the same `TokenSource func() (string, error)`
field `gitlab.Provider` has (ADR-0063), checked in both call sites that
set the `Authorization` header (`do`, the REST path, and `doGraphQL`).
`buildProviders`'s `gh`-fallback branch now passes `TokenSource: func()
(string, error) { return github.LoadGhToken("") } }` instead of a cached
string — no separate poke wrapper needed, since `LoadGhToken` already
shells out live on every call.

`Config.Token` (the `GITHUB_TOKEN` env-var / PAT path) is untouched, same
reasoning as the GitLab side: static PATs don't rotate on their own.

## Alternatives considered

- **Add a `RefreshGhToken` wrapper mirroring `RefreshGlabToken`.**
  Rejected — there's nothing to poke; `LoadGhToken` already shells out to
  `gh` on every call, so wrapping it in another `gh` invocation would just
  double the subprocess cost for no behavioural gain over calling
  `LoadGhToken` directly as the `TokenSource`.

## Consequences

- The GitHub provider's OAuth path no longer requires a daemon restart to
  pick up a token `gh` has refreshed in the background — mirrors the fix
  ADR-0063 gave the GitLab side.
- This ADR does not independently verify that `gh auth token` actually
  performs a live refresh-if-needed check the way `glab api user` was
  directly confirmed to (ADR-0065) — it relies on `LoadGhToken`'s existing
  code comment describing `gh auth token` as the "supported surface" for
  reading gh's current token. If GitHub-side token staleness is ever
  observed in production the way the GitLab case was, that assumption is
  the first thing to verify (e.g. by checking whether `gh auth token`
  alone, run stand-alone after a long idle period, returns a token that
  actually works against a live API call).
