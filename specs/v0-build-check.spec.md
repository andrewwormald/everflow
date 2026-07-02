---
goal: "Add a top-level Makefile target that builds + tests both the main module and the archived _v0 module"
provider: github
project: andrewwormald/everflow
runner: claude
base_branch: main
base_repo: /Users/andreww/dev/everflow
concurrency: 1
draft_mrs: true
status: ready
---

# Guard the `_v0/` archived module from silent rot

`_v0/` is the scheduled-skill PoC from before the v1 rewrite (see ADR-0019 and the README's Repository layout table). It lives in this repo as a separate Go module (`github.com/andrewwormald/everflow-v0`) for historical reference. Today nothing in our regular build path touches it — `go build ./...` from the root only walks the main module, so if a refactor breaks `_v0/`, we wouldn't know until someone notices.

The recent customer-arch scrub edited files inside `_v0/` without running `go build` against it. It happens to still compile today; that's lucky.

## What to add

Create a top-level `Makefile` at the repo root with these targets:

- `make check` — runs **everything that should be green before a commit**:
  - `go build ./...` (main module)
  - `go test ./...` (main module)
  - `go vet ./...` (main module)
  - `(cd _v0 && go build ./... && go test ./... && go vet ./...)` — the v0 module
- `make check-v0` — just the `_v0/` build + test + vet step, for when only the archive needs verifying
- `make check-main` — just the main-module steps, for fast local iteration
- `make help` (or default target) — print the available targets with one-line descriptions

The point is that `make check` is the single command a contributor can run before committing to be confident neither module is broken.

## Constraints

- Only the `Makefile` is added/modified. Nothing else.
- Don't add CI — the user explicitly opted to not add GitHub Actions in this dogfood spike.
- The Makefile must work on macOS bash and Linux bash. Use tabs for command lines (mandatory in GNU make).
- Each target should print what it's about to do (echo before each step) so failures are diagnosable.
- If `_v0/` ever fails to build, the relevant target must exit non-zero — don't swallow errors.

## Branch / MR

Native everflow branch naming is fine. One MR, scoped to just the Makefile addition.

## Done when

- `Makefile` exists at the repo root
- `make check` passes on the current `main` branch (both modules build + test + vet cleanly)
- `make check-v0` and `make check-main` each work in isolation
- `make help` (or running `make` with no args) prints the available targets
