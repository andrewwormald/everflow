# everflow v0 (archived)

The scheduled-skill PoC from the original design pass. Demonstrates the durable-workflow primitives (worktree isolation, `AddTimeout`-driven cycle, runner abstraction) on a simple `Initiated → Idle ⇄ Running` state machine.

**Not the current product.** The v1 mandate is bulk-refactor sweeps — see [`../DESIGN.md`](../DESIGN.md) and [ADR-0014](../decisions/0014-refactor-sweep-mandate.md). This directory is preserved as reference for how the workflow library composes.

Has its own `go.mod` (module name `github.com/andrewwormald/everflow-v0`) so it builds independently from the v1 root.

## Build / run

```bash
cd _v0
go build -o everflow-v0 .
./everflow-v0 runners              # → claude, mock

./everflow-v0 \
    --skill   "/dummy"           \
    --runner  mock               \
    --interval 5s                \
    --base-repo ~/dev/some-repo  \
    --root    /tmp/everflow-v0-wt
```

## Files

| File | Purpose |
|---|---|
| `main.go` | CLI flag parsing, daemon entrypoint, signal handling |
| `agent.go` | `AgentState`, `AgentStatus`, workflow builder, step functions |
| `runner.go` | `Runner` interface, registry |
| `claude.go` | `claude -p` runner |
| `mock.go` | No-op runner for demos and tests |
| `worktree.go` | `git worktree add`, `fetch && reset --hard`, removal |

Directory prefix `_` makes Go's toolchain skip this directory during the root module's `go build ./...` — it's a self-contained module that only builds when you `cd _v0` first.
