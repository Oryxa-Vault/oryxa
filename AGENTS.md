# AGENTS.md

Written for a coding agent. Everything here is checkable — run the command and
read the output rather than trusting this file.

Two jobs bring an agent here:

- **[Connect an agent to Oryxa](#connect-an-agent)** — the common one. One YAML
  file, no Go.
- **[Work on the core](#work-on-the-core)** — the Go server itself.

---

## What Oryxa is

A room that several people and several agents share. People talk to the room;
Oryxa drives the agents in it and streams everything back live.

```
  alice   bob   carol          many people, one room
    └──────┼──────┘
           ▼
    ┌─────────────┐
    │   Oryxa     │  owns: input queue, event log, presence, streaming out
    └──────┬──────┘
           │ connector (declarative HTTP)
           ▼
    ┌─────────────┐
    │ your agent  │  keeps: prompts, tools, memory, orchestration
    └─────────────┘
```

To the framework, Oryxa is one user. Nothing about the agent changes — no SDK,
no prompt edit, no awareness that Oryxa exists.

---

## Setup

Needs Go 1.25+. Docker optional.

```bash
git clone https://github.com/Oryxa-Vault/oryxa && cd oryxa
go build -o oryxa ./cmd/oryxa
go run ./cmd/mockagent &          # stand-in agent on :9000
./oryxa serve                     # API + viewer on :8080
```

Verify — this should print `ok` and a probe result:

```bash
./oryxa check mock
```

If it does, the install is good. Open http://localhost:8080 for the viewer.

---

## Connect an agent

A connector is a **description of HTTP calls**, not code. Write one file, probe
it, adjust, repeat. `oryxa check` runs a real turn and names the failure, so the
loop converges in two or three passes.

### 1. Write the file

`connectors/my-agent.yaml` — the minimum that can work:

```yaml
name: my-agent
base: http://localhost:5000

turn:
  method: POST
  path: /run
  body:
    prompt: "{{input}}"
  response:
    format: json
    text:
      - $.output          # tried in order, first match wins
```

### 2. Probe it

```bash
oryxa check my-agent
```

Reads files only — no server needed. It reports reachability, the captured
handle, timing, whether your selectors matched, and warnings for the ways a
connector can pass while being quietly wrong.

### 3. Read the failure and adjust

| `check` says | It means | Fix |
|---|---|---|
| connection refused | the agent isn't running, or `base` is wrong | `oryxa which my-agent` prints where it resolves to |
| no text matched | the request worked; your selector didn't | print the raw body, then point `text:` at the right path |
| empty answer, no error | selector matched an empty string | check `when:` isn't excluding the chunks that carry text |
| 401 / 403 | auth missing | `headers:` on the connector; `{{env.VAR}}` reads the environment |

Then put it in a room:

```bash
oryxa open my-agent
```

### Variables

| | |
|---|---|
| `{{input}}` | what the room said this turn |
| `{{handle}}` | the conversation id, if the agent has conversations |
| `{{context}}` | the room's shared state, rendered |
| `{{context.pinned}}` | just the curated set |
| `{{env.NAME}}` | an environment variable |

### The shapes that need more than the minimum

Full recipes in **[docs/integrating.md](docs/integrating.md)**:

| Your agent | Section |
|---|---|
| streams SSE | [Streaming](docs/integrating.md#2-streaming-sse) |
| has conversations / threads | [Conversations](docs/integrating.md#3-your-agent-has-conversations) |
| is a reasoning model | [Reasoning models](docs/integrating.md#4-reasoning-models) |
| streams deltas *and* a final message | [Deltas and finals](docs/integrating.md#5-your-agent-streams-deltas-and-a-final-message) |
| is a command, not a server | [Commands](docs/integrating.md#6-your-agent-is-a-command-not-a-server) |

`connectors/` has seven working examples against real frameworks. Read the one
closest to your agent's shape before writing from scratch —
[`connectors/templates/`](connectors/templates) has starting points.

---

## Connect a coding agent (Claude Code, Codex)

These have no HTTP surface — they read stdin and write JSON lines.
[`oryxa-shim`](cmd/oryxa-shim) puts a server in front of one, and then it is an
ordinary connector.

```bash
export ORYXA_SHIM_TOKEN=$(openssl rand -hex 32)
oryxa-shim -agents shim.yaml &     # 127.0.0.1:8090
oryxa serve &                      # same shell — both processes need the token
oryxa open claude-code codex
```

Set on only one side, the server starts, every agent looks healthy, and every
turn fails with a 401 that names neither end.

**Read this before pointing it at a repository you care about.** Both shipped
agents can write inside their working directory, and they are confined by
different mechanisms:

| | Claude Code | Codex |
|---|---|---|
| confinement | path patterns only — `Edit(./**)`, `Write(./**)` | OS sandbox — `-s workspace-write` |
| remove it and | it writes anywhere (verified: it wrote to `/tmp`) | the sandbox still holds |
| session id | you name it (`session: generate`) | it mints one (`session: capture`) |

Anyone who can reach that room can cause writes to that directory. Point it at a
checkout you can throw away. What may run lives in
[`shim.yaml`](shim.yaml) and can never be set over HTTP — that is why the shim
is a separate process.

---

## Work on the core

```bash
go build ./...
go test ./...
go test -race ./...        # the session loop is concurrent; this is the one that matters
go vet ./...
gofmt -l .                 # must print nothing
```

CI also runs `staticcheck` and a test that fails if `openapi.yaml` and the
registered routes disagree in either direction.

### Layout

```
cmd/oryxa/          server + CLI
cmd/oryxa-shim/     HTTP in front of agents that only speak stdin/stdout
cmd/mockagent/      stand-in agent for testing
internal/connector/ spec, templating, selectors, HTTP executor, check
internal/session/   the room: membership, lanes, turn loop
internal/events/    append-only log
internal/api/       HTTP surface
internal/sharedctx/ shared context
client/             the one exported Go package, a thin wrapper over /v1
connectors/         connector files, loaded at startup
shim.yaml           what the shim may run — never settable over HTTP
openapi.yaml        the HTTP contract, tested against the handlers
```

### Invariants — do not break these

These are what make "works with any framework" true rather than aspirational.
CI enforces the first one.

1. **The core never branches on which framework it is talking to.** No
   framework name appears in `internal/`.
2. **Multi-user, not multi-agent.** Their orchestration stays theirs.
3. **Never look inside the agent.** Text is text; everything else is recorded
   whole as opaque activity and never interpreted. Parsing a framework's event
   schema couples us to its release cycle.
4. **Never touch prompts.**
5. **Events are the truth.** Anything derived is derived. If a feature wants to
   write to a projection, the answer is an event.
6. **The log is append-only.** Never coalesce, never dedupe.
7. **Capabilities are declared, never assumed.** No fake streaming.
8. **One turn at a time per agent, parallel across agents.** An agent's own
   conversation is sequential, so overlapping its turns would misrepresent what
   is underneath. Two agents have two conversations and no reason to wait.

[`PLAN.md`](PLAN.md) is the source of truth for what is built and what is not.

---

## Facts worth not guessing

| | |
|---|---|
| Go | 1.25+ |
| server | `:8080` — API and viewer |
| shim | `127.0.0.1:8090` |
| mockagent | `:9000` |
| verified against | Claude Code 2.1.208, codex-cli 0.147.0 |
| turn budgets | 30/min per room, 120/min per server, `0` disables |
| store | in-memory unless `-db` names a postgres DSN |
| auth | off unless `-token` is set; startup banner says which |
| licence | Apache 2.0 |

Every command takes `-json`. Room commands take `-server` and `-token`, falling
back to `ORYXA_URL` and `ORYXA_TOKEN`; connector commands take `-connectors`,
falling back to `ORYXA_CONNECTORS`.

## Where else to look

| | |
|---|---|
| [README.md](README.md) | the short version, for humans |
| [docs/tutorial.md](docs/tutorial.md) | your agent in a room, end to end |
| [docs/integrating.md](docs/integrating.md) | the full connector reference |
| [docs/running.md](docs/running.md) | durability, auth, identity, budgets |
| [SECURITY.md](SECURITY.md) | the posture, and what it does not cover |
| [PLAN.md](PLAN.md) | what is built, what is not, and why |
| [skills/](skills/) | the same knowledge packaged as agent skills |
