# Agent guide

Instructions for AI agents (Claude Code, Codex, OpenHands, ...) working in this
repository. Read this first before making changes.

## Read these before contributing

1. **[README.md](README.md)** — what everflow is and what works today
2. **[DESIGN.md](DESIGN.md)** — full architecture and roadmap
3. **[decisions/](decisions/)** — every meaningful decision and its rationale. Read the index, then any ADRs touching the area you're changing.

The repo is dogfooded for "long-running AI agent loops" — it should be readable
and contributable *by an agent*. The decisions log is the primary mechanism for
that; it captures context that isn't visible in the code alone.

## Working rules

### Decisions get ADRs

Every meaningful decision is captured as an Architecture Decision Record under
`decisions/`. "Meaningful" means: it would surprise someone reading the code,
it locks out alternatives, or it would be re-litigated without a written
record. Examples:

- Picking a primitive (workflow library, runner shape, IPC mechanism)
- Scope decisions (what's in v1, what's deferred)
- Naming/structural choices that affect contributors
- Tradeoffs where the alternatives were close

Trivial decisions (variable naming, gofmt-fixable nits, choosing one of several
equally good standard-library options) do *not* need ADRs.

When writing an ADR:

1. Number it as the next unused integer, 4-digit zero-padded (`0013-...`).
2. Use the template in [`decisions/README.md`](decisions/README.md).
3. Update the index in `decisions/README.md`.
4. If superseding a prior ADR, mark the old one `Superseded by ADR-NNNN` and link.

### Code rules

- Run `go vet ./...` and `go build ./...` before committing.
- Before committing, run `go test ./...` and `(cd _v0 && go test ./...)` to verify both modules build cleanly.
- No `_test.go` for the PoC unless you're adding new behavior worth covering.
- Don't add dependencies casually. Workflow (durable state) and `k8s.io/utils/clock` (clock injection) are the only required ones today.
- Keep `main.go` small. Anything reusable goes in a focused file (`runner.go`, `worktree.go`, ...).
- Single-line comments only unless the *why* genuinely needs more.

### Don't do these without explicit human approval

- Force-push to `main`
- Add a runtime dependency on a hosted service
- Re-introduce the `Plan/Act/Observe/Reflect` step graph (see [ADR-0010](decisions/0010-simplified-step-graph.md) for why this was collapsed)
- Modify `LICENSE`

### Commit hygiene

- One logical change per commit.
- Commit message subject describes intent, not just diff (`docs: record ADR-0013 on sqlite store choice`, not `add file`).
- Co-author trailer when an AI agent contributed.

## How to find context

| Question | Where to look |
|---|---|
| Why was X chosen? | `decisions/` — grep title or read the index |
| What does Y do? | Source comments; if missing, the code itself |
| What's planned but not built? | `DESIGN.md` (vision) + open ADRs marked `Proposed` |
| How do I run this? | `README.md` |
| What's the L1/L2/L3 mapping? | `DESIGN.md` § *L1/L2/L3 → workflow mapping* |
