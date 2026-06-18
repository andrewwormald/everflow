# ADR-0020: GitLab provider — hand-rolled REST client, bare-token webhooks

**Status**: Accepted
**Date**: 2026-06-18

## Context

[ADR-0014](0014-refactor-sweep-mandate.md) names refactor sweeps as the
primary use case; the first-class provider for that is GitLab (matches the
author's Luno work environment and the existing `mrs-babysit` skill that
inspired the project). Per [ADR-0007](0007-pluggable-runner-interface.md)
and the `Provider` interface defined in v1 scaffold, we need a concrete
GitLab implementation.

Three implementation choices to record:

## Decisions

### 1. Hand-rolled HTTP client, no Go SDK

We call GitLab's REST API directly using `net/http` + `encoding/json`.
**No dependency on `github.com/xanzy/go-gitlab`** or any other SDK.

Reasons:
- The interface surface we need is small (~10 endpoints).
- A hand-rolled client gives precise control over rate-limit / retry
  behaviour, which we'll want to tune for the daemon's specific access
  pattern (rare reads, bursty writes during MR processing).
- One less dependency to vet, upgrade, and worry about (an org running
  everflow inside corporate boundaries cares about supply-chain risk).
- The GitLab API is *stable* — `/api/v4/` has been the path for years —
  so the maintenance cost of pinning to v4 ourselves is low.

The implementation is ~300 LOC for the full Provider interface.

### 2. Webhook verification: bare-token comparison via `X-Gitlab-Token`

GitLab's webhook security model is **not HMAC**. The platform sends the
configured secret back verbatim as the `X-Gitlab-Token` header on every
delivery. We compare the header value to the registered secret using
`crypto/subtle.ConstantTimeCompare`.

This is weaker than GitHub's HMAC-SHA256 signing of the body
(`X-Hub-Signature-256`), but it's all GitLab offers in the standard
webhook config. Practical mitigations:

- **Per-Run secrets**: every Run registers its own random secret with
  GitLab, scoped to the `webhook.SecretRegistry`. A leaked secret only
  exposes one Run; cleanup is automatic on Run termination.
- **TLS required**: the `--public-base-url` must be `https://`. We do
  not document an http-only path.
- **No payload trust without verification**: the webhook server returns
  `401` for any POST whose `X-Gitlab-Token` doesn't match.

The `Provider.VerifySignature` interface signature accepts `body []byte`
even though GitLab ignores it; that keeps the interface uniform across
providers and lets the GitHub adapter (v2) use HMAC over the body.

### 3. Project ID accepted as either numeric or path-with-namespace

GitLab's API accepts both `/api/v4/projects/123` and
`/api/v4/projects/group%2Fproject` (URL-encoded path). We use
`url.PathEscape(projectID)` and pass through whatever the caller gave us.

Inbound webhook payloads expose `project.path_with_namespace`, which we
prefer over the numeric ID because:
- It's stable across project moves (changing namespace updates the path;
  moving between groups changes the path but also the numeric ID).
- It's human-readable in logs (`lunomoney/core` vs `42`).
- It matches what users type at `everflow start --project ...`.

The `gitlabProject.idAsString()` helper picks the path when available
and falls back to numeric ID. Callers store whatever they get in
`AgentState.ProjectID` and use the same string for subsequent API calls.

## Alternatives considered

- **Use the official `go-gitlab` SDK**: bigger surface, more transitive
  deps, slightly less control over retry/rate-limit behaviour. Saves
  ~200 LOC. Net negative for v1; revisit if our hand-rolled client grows
  past ~600 LOC.
- **Require numeric project IDs only**: simpler, but a worse UX. The user
  types `lunomoney/core`; making them look up `42` is friction.
- **Use HMAC on top of GitLab's bare token**: GitLab doesn't offer this;
  we'd have to ask users to wrap their webhook in a custom proxy that
  adds HMAC. Out of scope. Per-Run secrets + TLS is enough.
- **Wait for GitHub before designing the abstraction**: rejected. The
  Provider interface is already locked by v1 scaffold; we validate it
  by shipping GitLab first and GitHub second. If the interface needs
  refactoring after GitHub lands, we change it then.

## Consequences

- Authentication is via the `GITLAB_TOKEN` env var at daemon start. Per
  ADR-0017 the daemon's token determines the default Author; the
  `--author` override at `everflow start` is the escape hatch for shared
  deployments.
- `--gitlab-base-url` is a daemon flag so the same binary works against
  gitlab.com (default) and self-hosted instances (e.g. `https://gitlab.luno.com`).
- The `apiError` type carries the response body up the call stack. Future
  work: classify common GitLab error patterns ("token expired", "rate
  limited", "webhook URL unreachable") for nicer messaging — not v1.
- Rate limiting: the v1 client makes no attempt to throttle itself. We
  assume the steady-state load (one webhook arriving every few minutes
  per Run, a handful of API calls per state transition) is well under
  GitLab's per-token limit. If a refactor with high concurrency hits
  limits, we add a token-bucket later behind the existing client surface.
- The `Provider` interface is now empirically validated by one
  implementation. v2 (GitHub) is the second test. Any awkwardness we
  hit implementing GitHub may force interface revisions — those will be
  recorded as supersession of relevant ADRs.
