# ADR-0021: GitHub provider — HMAC webhooks, three comment events collapse to one

**Status**: Accepted
**Date**: 2026-06-19

## Context

[ADR-0020](0020-gitlab-provider-impl.md) shipped GitLab as the first
`provider.Provider` implementation. The second test of the interface is
GitHub — and a good test, because GitHub's API and webhook model differ
from GitLab's in several concrete ways. Surviving both validates the
interface; awkward bits inform interface revisions.

This ADR records the GitHub-specific implementation choices that differ
from GitLab.

## Decisions

### 1. HMAC-SHA256 over the body, not a bare token

GitHub signs every webhook delivery with `X-Hub-Signature-256: sha256=<hex>`,
where the digest is HMAC-SHA256 of the raw body keyed by the secret we
registered. This is *real* cryptographic verification: even if the secret
leaks in transit (it doesn't — TLS), the body cannot be tampered with
without recomputing the HMAC, which requires the secret.

This is materially stronger than GitLab's bare-token model
([ADR-0020](0020-gitlab-provider-impl.md) §2). We implement it correctly:

- Use `crypto/hmac` and `crypto/sha256` from the standard library
- Use `hmac.Equal` for constant-time comparison (not `bytes.Equal`)
- Validate the `sha256=` prefix before hex-decoding
- Hex-decode the header and compare the raw bytes, not the hex strings
  (avoids subtle bugs around case-sensitivity in hex)

The `Provider.VerifySignature` interface already accepts the body as a
parameter, so no interface change was needed.

### 2. `owner/repo` split from a single opaque projectID string

GitHub's API uses two-part paths: `/repos/{owner}/{repo}/...`. To keep
the `Provider` interface uniform with GitLab (which uses a single
project identifier), we accept `"owner/repo"` as the opaque projectID
string and split it once at the API boundary in `splitProjectID`.

Trade-off:
- The caller (workflow, CLI, AgentState) sees one string.
- The provider knows it has two components.
- An invalid input like `"just-the-repo"` fails fast with a clear error.

The webhook payloads carry `repository.full_name` ("owner/repo") which
we use as the canonical ProjectID for round-tripping back to the API.

### 3. `check_suite` is our "pipeline" abstraction (not `workflow_run`, not `check_run`)

GitHub has multiple CI-completion events:

- `workflow_run` — fires for GitHub Actions specifically
- `check_run` — individual jobs (GHA + third-party)
- `check_suite` — aggregate of all check_runs for a commit

For our purposes (knowing "did CI as a whole succeed or fail for this PR's
head commit?"), **`check_suite` is the right abstraction**. It covers:

- GitHub Actions
- External CI that integrates via the Checks API (Circle, Buildkite, Jenkins
  plugins, etc.)
- Any future tools that publish check_runs

`workflow_run` would have locked us into GitHub Actions only.
`check_run` is too granular — we'd see N events per CI execution and
have to aggregate ourselves.

The pipeline event payload exposes only the aggregate (`conclusion:
success|failure|cancelled|timed_out`). If we need individual job details
(for the cheap CI-classifier filter), we fetch them lazily via
`GET /repos/.../check-runs?check_suite_id=N` — only when the filter
decides we actually need them.

### 4. Three comment events collapse onto `provider.EventNoteAdded`

GitHub distinguishes three kinds of MR-thread interaction:

- `issue_comment` (action=created with `issue.pull_request != null`) — top-level conversation comments
- `pull_request_review` (action=submitted with a body) — a review with prose feedback
- `pull_request_review_comment` (action=created) — inline comments tied to specific code lines

For everflow's purposes — "the human said something substantive about this
MR that I should consider" — these are the same. They all become
`EventNoteAdded`. The filter sees the comment body; it doesn't care
whether the body came from a top-level note, a review summary, or an
inline annotation.

We subscribe to all three at webhook-registration time
(`mapEventKinds`), normalise the differences during event parsing, and
present a unified `provider.Note{ID, Body}` to the filter and workflow.

One specific exception: **body-less "approved" reviews are skipped**.
A review where the user clicked "Approve" without typing anything is
not a comment requiring agent attention. We return `provider.ErrIgnore`
on those; the webhook server drops them silently.

### 5. Self-emitted PR opens are explicitly ignored

When the workflow opens an MR, GitHub sends us back a `pull_request`
event with `action=opened`. We ignore it: by the time the event arrives,
our state machine is already in `StatusAwaitingMerge` for that unit;
reacting to "we just opened this" is meaningless.

We do the same in the GitLab adapter, for the same reason. Worth noting
both adapters arrived at this independently — it's a real category of
events, not a quirk.

## Alternatives considered

- **Use `go-github` SDK** — same trade-offs as ADR-0020 for `go-gitlab`.
  Saves ~250 LOC, adds ~30 transitive deps. Net negative for v1. Revisit
  if the hand-rolled client exceeds ~600 LOC.
- **Subscribe to `workflow_run` instead of `check_suite`** — locks us
  into GitHub Actions. Most repos use third-party CI through the Checks
  API; `check_suite` covers both uniformly.
- **Treat the three comment events as distinct `EventKind`s** — would
  require the filter author to handle three near-identical branches.
  Collapsing at the provider boundary is the right normalisation.

## Consequences

- Empirically validated: the `Provider` interface survives a second
  implementation without changes. Adding a third provider (Bitbucket,
  Gitea, Forgejo) is now mechanical work — ~300 LOC each, following the
  GitLab/GitHub template.
- `RetryPipelineJob` on GitHub works only for GitHub Actions jobs. Third-
  party CI integrations expose check_runs but no rerun endpoint. If a
  Run's CI is non-Actions, the deterministic flake-retry path falls back
  to "ask the author" via `StatusPaused` and a comment.
- Webhook registration on a GitHub repo requires `admin:repo_hook`
  scope on the token (classic PAT) or the equivalent fine-grained
  permission. Documenting this is README work, not v1 code.
- `IsBot` handles two heuristics: `User.Type == "Bot"` (set on GitHub
  Apps) and the `*[bot]` username convention (used by dependabot, renovate,
  codecov, etc.). The mirror exists in the GitLab adapter as a `Bot`
  field on user metadata, but the `*[bot]` convention is GitHub-only.
- Failure to provide a PR-style projectID (e.g. someone passes a numeric
  repo ID) fails at the first API call with a clear error from
  `splitProjectID`. We could validate at `everflow start` time too; not v1.
- The webhook event-set sent to GitHub (`mapEventKinds`) is the union
  of GitHub event names each `provider.EventKind` translates to. If a
  user subscribes only to `EventNoteAdded`, we subscribe to all three of
  `issue_comment` / `pull_request_review` / `pull_request_review_comment`.
  This is more chatty than strictly necessary; we filter on receive.
