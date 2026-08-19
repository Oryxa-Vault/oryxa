# Rust rewrite

Rust owns the server, the `/v1` API and everything a person types. One binary
serves rooms, opens the interactive room view, and answers to a script; the
browser viewer remains embedded in it. What is left in Go is `oryxa-shim`, and
that is an intentional boundary rather than unfinished migration work.

## Implemented in Rust

- YAML/JSON connector loading, validation, templates, selectors and conditions
- JSON, SSE and NDJSON execution, open/capture, streaming parts and checks
- one sequential lane per agent, with parallel agents and input coalescing
- wake decisions, cancellation, close, withdrawal and per-person room keys
- append-only memory and PostgreSQL event stores
- restart recovery by folding rooms, outputs, access, context and lane cursors
- shared append/value context, optimistic concurrency, pins and bounded render
- automatic shared-context rollups through an ordinary summariser connector
- `/v1` HTTP API, SSE replay/follow stream, authentication and embedded viewer
- durable API connector registration, public-network confinement and safe delete
- per-room and server-wide turn budgets
- `oryxa serve`, connector inspection/check/wake commands, and `mockagent`
- room commands (`open`, `send`, `tail`, `replay`, `sessions`, `context`, `key`
  and `approve`)
- the interactive room view, including the local runtime it starts when there is
  no server to attach to, and ACP permission prompts
- the installer's release binaries, which are this crate and nothing else

## Intentionally retained outside Rust

- `oryxa-shim`, which launches command-line coding agents. It exists to start
  processes on the host, which is why it is also absent from the Docker image.
- the existing embedded frontend asset
- `client/`, the Go API client, and the `internal/` server it tests against

The Rust server can be exercised completely through the stable HTTP API, the
browser viewer, or the room view. `openapi.yaml` is checked against the Rust
router by `tests/openapi.rs`, so a route cannot be added without documenting it.
CI formats, tests and lints the supported language trees at this boundary.

## Verification

```bash
cargo fmt --all --check
cargo test --locked --all-targets
cargo clippy --locked --all-targets -- -D warnings
```
