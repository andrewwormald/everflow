# ADR-0019: Project layout — `main.go` at root, business logic under `internal/`

**Status**: Accepted
**Date**: 2026-06-17

## Context

The v1 implementation starts now ([ADR-0014](0014-refactor-sweep-mandate.md)
through [ADR-0018](0018-starlark-filter-and-phrase-learning.md) lock the
design). Before writing any business logic, we need a project layout that:

- Lets `go install github.com/andrewwormald/everflow@latest` work — i.e.
  the binary's main package needs to be discoverable from the module root.
- Keeps packages cohesive (one concept per directory) so an AI agent
  picking up the repo cold can find code by topic.
- Preserves the v0 scheduled-skill code as buildable reference without it
  polluting the v1 build.
- Avoids the `pkg/` vs `internal/` Go-community debate by picking and
  recording the answer.

## Decision

```
github.com/andrewwormald/everflow/
├── main.go                     # CLI entry point; thin
├── go.mod, go.sum
├── README.md, DESIGN.md, AGENTS.md, LICENSE
├── decisions/                  # ADRs
├── logo/                       # brand assets
├── _v0/                        # archived scheduled-skill PoC
│   ├── go.mod                  # own module: github.com/andrewwormald/everflow-v0
│   └── *.go
└── internal/
    ├── provider/               # Provider interface + Event/User/MR types
    │   └── gitlab/             # (v1 follow-up) GitLab impl
    │   └── github/             # (v2) GitHub impl
    ├── refactorsweep/          # AgentState, AgentStatus, workflow state machine
    ├── runner/                 # Runner interface + registry
    │   └── claude/             # (v1 follow-up) claude -p adapter
    ├── filter/                 # Filter interface + Starlark eval
    ├── webhook/                # HTTP server for inbound webhooks
    └── store/                  # RecordStore + TimeoutStore wiring
```

Three load-bearing pieces:

1. **`main.go` at the module root**. Users run `go install
   github.com/andrewwormald/everflow@latest` and get a binary named
   `everflow`. If we used `cmd/everflow/main.go` instead, the install URL
   becomes `github.com/andrewwormald/everflow/cmd/everflow@latest` — uglier
   for users, no real win.

2. **Everything substantive under `internal/`**. We are not currently a
   library for others to import; we're a daemon + CLI. `internal/` makes
   that intent explicit (Go's toolchain blocks external imports of
   `internal/...`), which gives us freedom to refactor without worrying
   about downstream consumers.

3. **`_v0/` as a self-contained module**. The leading underscore tells
   Go's toolchain to skip the directory when building the root module
   (`go build ./...` from root does not descend). The directory has its
   own `go.mod` with module path `github.com/andrewwormald/everflow-v0`,
   so it builds independently with `cd _v0 && go build .`. This preserves
   the v0 reference code without it cluttering v1 imports or `go vet`.

## Alternatives considered

- **`cmd/everflow/main.go` + library packages at the root**: the canonical
  Go-library layout (cobra, kubectl, etc. use it). Rejected for v1 because
  everflow is a binary, not a library — there's no story for `import
  "github.com/andrewwormald/everflow/something"` from external code.
  We can adopt it later if everflow ever exposes a Go API.
- **`pkg/` instead of `internal/`**: `pkg/` is a community convention with
  no toolchain enforcement; `internal/` has Go's compiler enforcing it.
  Since we explicitly do *not* want external imports of these packages
  in v1, `internal/` is the right tool.
- **Delete `_v0/` entirely**: tempting (less code) but [ADR-0010](0010-scheduled-skill-poc-first.md)
  explicitly preserves the scheduled-skill PoC as reference. The cost of
  keeping it (a sibling go.mod, a `_` prefix) is small.
- **Flatten — put everything in the root package**: works for ~500 LOC,
  rots fast as we add a provider, a runner, a webhook server, a filter
  engine, and a store. The split now is cheaper than the split later.

## Consequences

- Adding a new package = a new directory under `internal/`. Adding a new
  *provider* or *runner* = a new sub-directory under
  `internal/provider/...` or `internal/runner/...`, alongside an
  `init()`-registered implementation of the parent interface.
- All non-trivial business logic lives under `internal/`. `main.go` stays
  thin — flag parsing, dependency wiring, signal handling, command
  dispatch. If it grows past ~300 LOC, refactor into `internal/cli/`.
- The `_v0/` reference code is not maintained alongside v1 changes. If a
  v1 ADR supersedes a v0 behaviour, the v0 code may stay frozen
  intentionally as a "this is what the prior design looked like" artefact.
- Should everflow ever need to expose a public Go API (programmatic
  Trigger from another binary, embedding the workflow runtime in someone
  else's app), the relevant pieces graduate from `internal/` to a public
  package path. That decision will need its own ADR.
