# ADR-0028: `everflow start` triggers via a localhost-only HTTP endpoint

**Status**: Accepted
**Date**: 2026-06-22

## Context

The daemon owns the workflow runtime. The CLI (`everflow start`) lives in
a separate process and needs a way to hand it a new Run. Options:

- **Shared sqlite** — CLI writes a record directly. Bypasses the
  workflow library's Trigger; ends up reinventing event publication +
  the outbox. Bad.
- **Unix domain socket** — works on macOS/Linux. Adds platform-specific
  paths and breaks on Windows.
- **A "trigger" directory** the daemon polls for new Run requests as
  files. Latency, race conditions, hard to surface errors back.
- **HTTP endpoint on the daemon** — simple, well-understood, the daemon
  already runs an HTTP server for webhooks.

HTTP wins. But the daemon's existing listener is exposed at
`--public-base-url` so providers can deliver webhooks. Mounting
`/trigger` on the same listener would let anyone on the internet start
Runs — a serious authorisation hole given everflow can push to
provider repos and spawn LLM subprocesses.

## Decisions

### 1. Two listeners on the daemon

- **Public listener** (`--listen`, default `:8080`) — `/webhook/*` and
  `/health`. This is what the provider's webhook URL points at and what
  ngrok/Tailscale/EC2 DNS exposes.
- **Local listener** (`--trigger-listen`, default `127.0.0.1:8081`) —
  `/trigger`. Bound to loopback, never exposed to the public URL.

main.go starts two `http.Server` instances, both share `ctx` for
shutdown. The webhook package's `Server` exposes a `Mount(mux)` method
(instead of owning a `Listen` loop) so the daemon can compose routes
freely.

### 2. JSON request/response on `/trigger`

```
POST /trigger
Content-Type: application/json

{
  "mode": "spec" | "sweep",      // optional; inferred from spec vs units
  "goal": "...",
  "provider": "gitlab",
  "project": "owner/repo",
  "runner": "claude",
  "base_repo": "/home/.../core",
  "base_branch": "main",          // optional; defaults to "main"
  "concurrency": 1,               // optional; defaults to 1
  "units": ["svc-a", "svc-b"],    // sweep mode
  "spec_path": "...",             // spec mode
  "spec_body": "..."              // spec mode (markdown body)
}

Response:
200 OK
{"run_id": "<uuid>", "foreign_id": "<uuid>"}
```

The daemon assigns `foreign_id` (auto-generated UUID if the CLI didn't
provide one). The workflow library assigns `run_id`. Both are returned
so the CLI can echo them; future `everflow status <runID>` (not in v1)
takes them as input.

### 3. CLI is a thin POSTer; spec parsing on the CLI side

`everflow start`:

1. Parses `--spec` or `--units` (mutually exclusive)
2. If `--spec`: reads + parses the markdown, builds the request body
   from the frontmatter (with flag overrides)
3. If `--units`: builds a sweep-mode request from the CSV + flags
4. POSTs to `--daemon` (default `http://127.0.0.1:8081/trigger`)
5. Prints the run ID

Spec parsing happens in the CLI process, not the daemon, because:
- The spec file path is local to wherever the user runs `everflow start`
- If the daemon is on a different host, it can't read the file
- Frontmatter validation errors surface immediately to the user, not
  via the HTTP response

### 4. No authentication on `/trigger` for v1

Localhost-only is the security model. Production deployments running
the daemon on a remote VM should:

- SSH-tunnel the trigger port (`ssh -L 8081:127.0.0.1:8081 vm.example`)
- Or run `everflow start` on the VM itself
- Or — when v2 lands — use a shared-secret auth header configured at
  daemon start

Adding auth in v1 is overkill: the threat model assumes the daemon
host is trusted (it has the provider token and can push to the repo).
Anyone with shell on that host can already do anything.

### 5. The `/trigger` body is a separate `triggerRequest` type, not raw `AgentState`

```go
type triggerRequest struct { ... }  // exposed via JSON
```

Direct AgentState serialisation was rejected because:
- Many AgentState fields are workflow-managed (Author, WebhookID,
  History, etc.) — the trigger shouldn't accept them from outside
- AgentState's schema will evolve; the trigger contract should be
  stable separately
- Validation lives on `triggerRequest` (required-fields, mode
  inference); AgentState is built from a valid request

## Alternatives considered

- **Shared sqlite write** — bypasses workflow's Trigger semantics, no.
- **Unix domain socket** — works but adds OS-specific code; HTTP is
  uniformly testable.
- **Single listener with allow-by-source check** — `r.RemoteAddr` is
  spoofable via forwarded headers behind a proxy. Two listeners is the
  honest separation.
- **gRPC instead of REST** — overkill; one POST endpoint doesn't
  warrant the dependency.

## Consequences

- `webhook.Server.Listen` is gone; replaced by `Mount(mux)`. The
  webhook package now provides routes, not a server lifecycle. Callers
  (the daemon) own `http.Server`. Cleaner separation of concerns.
- Two `http.Server` instances mean two `ListenAndServe` goroutines and
  two `Shutdown` calls. Both wired up in main.go with a shared `ctx`
  for cancellation.
- Public listener defaults to `:8080` (all interfaces — needs to be
  reachable from the public URL). Local listener defaults to
  `127.0.0.1:8081` (loopback only).
- `everflow start --daemon URL` lets the CLI target a remote daemon
  (e.g. through an SSH tunnel) — the default just points at localhost
  on the conventional port.
- The CLI now imports `internal/spec` for parsing. ~150 LOC of new
  CLI code; tests are the smoke test against a running daemon
  (real-process round-trip is the only honest test).
- Smoke-tested: sweep + spec triggers both round-trip; `/trigger` on
  the public port returns 404; `/webhook` on the local port also 404.
