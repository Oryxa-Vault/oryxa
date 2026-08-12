# Oryxa

**Agent frameworks are single-user. Oryxa makes them multi-user.**

LangGraph, CrewAI and ADK already do multi-agent well. None of them let several
people share one agent session — watch it live, add input, hand off, pick up
where someone left off. That's the job.

> To the framework, Oryxa is one user. To your team, it's a shared room.

![Two agents answering one question at once](docs/media/room.gif)

*One question, two agents from rival labs, answering at the same time — the room
costs its slowest agent rather than the sum of them. Recorded from a real
session; the script that produces it is in [recording/](recording).*

Status: **v0.3** — connectors, rooms, shared context agents can read and write,
an event log everything is a fold over, live stream, viewer.

> **Run it locally, not on the open internet.** Rooms are scoped — each carries
> a secret, and one token no longer opens all of them — and turns are budgeted
> per room and per server. What is still open: identity is self-declared unless
> a proxy establishes it, so a name is a claim rather than a fact. See
> [SECURITY.md](SECURITY.md).

[![Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8.svg)](go.mod)
[![PRs welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

---

## Start here

```bash
docker compose up -d && go run ./cmd/mockagent &
open http://localhost:8080
```

That's it — a running room with a stand-in agent. Pick the agent in the sidebar
(clicking it runs a real probe turn and shows what came back), hit **+ new
session**, and talk to it.

No Docker:

```bash
go build -o oryxa ./cmd/oryxa && go run ./cmd/mockagent &
./oryxa launch window
```

**New here and want to help?** The most useful thing is a connector for a
framework we haven't tested — that's one YAML file, no Go required. See
[CONTRIBUTING.md](CONTRIBUTING.md); contributions are genuinely welcome.

**To see it as two people:** open the viewer in a second tab and change the name
in the composer. Both tabs are in the same room, watching the same agent.

**Several agents in one room:** click more than one agent before creating the
session. Each gets its own lane: its own cursor into what the room has said, its
own goroutine, its own conversation with its own framework. They answer **in
parallel**, so a room costs its slowest agent rather than the sum of all of them.
One agent failing does not stop the others.

```bash
curl -X POST localhost:8080/v1/sessions \
  -d '{"agents":["langgraph","adk","pydanticai","crewai"]}'
```

```bash
oryxa check langgraph          # probe a connector, no server needed
oryxa serve                    # API + viewer, no browser
```

Say something while an agent is mid-turn and it does not queue behind it: the
agent picks it up with its next turn, along with anything else said meanwhile.
Eight messages typed at human speed against a slow agent ran as two turns, not
eight. Anyone joining later replays the whole history and then follows live —
`?since=` makes late join, reconnect and replay the same code path.

**Not everyone answers everything.** A connector says what it engages on, and
the room works out who a message is for — an agent @mentioned or named, one that
declared an interest in something you said, nobody at all when you are asking a
colleague by name or just saying "thanks". Measured on seven live agents: 35
model calls instead of 133.

```bash
oryxa wake "we should consider using more ai tools"   # who answers, and why
```

---

## Connecting your agent

A connector is a **description of HTTP calls**, not code. Nothing in the core
knows any framework's name — that's what keeps both sides free.

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

Drop it in `./connectors/` (or `POST /v1/agents`), then:

```bash
oryxa check my-agent
```

`check` runs a **real** probe turn and reports what came back — reachability, the
captured handle, timing, whether your selectors matched, and warnings for the
ways a connector can pass while being quietly wrong. It's the fastest way to tune
one, and it answers *is it me or is it them* without starting a session.

**→ [Tutorial](docs/tutorial.md)** — your agent in a room, end to end.

**→ [Agent skills](skills/)** — let Claude write your connector. It's an
iterative probe-and-adjust loop, which is what agents are good at.

**→ [Integration guide](docs/integrating.md)** — recipes for streaming,
conversations, reasoning models and typed-event protocols; the full reference for
variables, selectors, `when:` and capabilities; and a symptom-to-fix table for
when it doesn't work.

### Verified connectors

All four run against real servers with a real model (`@cf/openai/gpt-oss-120b`
on Cloudflare Workers AI), including token-level streaming and multi-turn
continuity across several people.

| Connector | Framework | Shape |
|---|---|---|
| [langgraph.yaml](connectors/langgraph.yaml) | LangGraph | `langgraph dev` — per-thread URL, assistant id, SSE |
| [adk.yaml](connectors/adk.yaml) | Google ADK | `adk api_server` — global `/run_sse`, ids in body |
| [pydanticai.yaml](connectors/pydanticai.yaml) | Pydantic AI | hand-rolled FastAPI, SSE — the generic-http path |
| [crewai.yaml](connectors/crewai.yaml) | CrewAI | hand-rolled FastAPI, no streaming, multi-agent inside |
| [bee-agui.yaml](connectors/bee-agui.yaml) | AG-UI (any) | typed event protocol — tool calls, reasoning, state deltas |
| [claude-code.yaml](connectors/claude-code.yaml) | Claude Code | a **command**, not a server — NDJSON over stdout |
| [codex.yaml](connectors/codex.yaml) | Codex | a command too, and a different one in every detail |

Seven genuinely different surfaces, one core, one YAML file each.
`connectors/templates/` has starting points including plain HTTP.

CrewAI is the sharpest test of the boundary: a crew is multi-agent by design, so
one Oryxa turn becomes several LLM calls between the crew's own agents — and
Oryxa sees one turn with one result. Their orchestration stays theirs.

The AG-UI connector is for a *protocol* rather than a framework, so it works with
any AG-UI agent. It is also the tool-calling test: verified against a real agent
that made eight tool calls in one turn (`work_manager.plan`, `load_skill`,
`write_artifact`, …). Every `TOOL_CALL_START` / `ARGS` / `END` / `RESULT` and
every `STATE_DELTA` landed as opaque `activity` — none of it reached the answer,
and no new spec feature was needed for any of it.

### Command-line agents

Claude Code and Codex have no HTTP surface at all: they read stdin and write
JSON lines. [`oryxa-shim`](cmd/oryxa-shim) puts a server in front of one, and
then it is an ordinary connector reading an ordinary NDJSON stream — no new spec
feature, same as AG-UI.

```bash
oryxa-shim -agents shim.yaml     # 127.0.0.1:8090
oryxa check claude-code
```

The command line lives in [`shim.yaml`](shim.yaml), never in the connector, and
**that separation is the point**. Oryxa registers connectors over HTTP at
`POST /v1/agents`, so a spec is something a caller supplies at runtime. A spec
that could name a command to run would turn that endpoint into remote code
execution behind one shared token. The shim is a separate process so the only
thing deciding what may run is a file on the machine.

```yaml
agents:
  - name: claude-code
    dir: .
    session: generate      # we name the id; `capture` reads it back instead
    first:  [claude, -p, --output-format, stream-json, --verbose,
             --tools, "Read,Grep,Glob", --session-id, "{{handle}}"]
    resume: [claude, -p, --output-format, stream-json, --verbose,
             --tools, "Read,Grep,Glob", --resume, "{{handle}}"]
```

`ORYXA_SHIM_TOKEN` guards the shim, and the connectors send it as a bearer —
which means **both processes need it in their environment**, the shim to check it
and the server to render it into the request:

```bash
export ORYXA_SHIM_TOKEN=$(openssl rand -hex 32)
oryxa-shim -agents shim.yaml &
oryxa serve                       # same shell, same variable
```

Set on only one side, the server starts, every agent looks healthy, and every
turn fails with a 401 that names neither end.

> **Both shipped agents can write inside their working directory, and this is
> the line to think about before copying them.** The reason is that read-only
> made them worse colleagues than they needed to be: asked what was wrong with
> this repo, Codex found a real crash in the event fan-out and could only report
> it as a suspicion, because `go test` could not create its build directory.
> Verifying is most of what a coding agent is for.
>
> **They are confined differently, and the difference matters.** Codex is held
> inside its workspace by the OS sandbox (`-s workspace-write`). Claude Code is
> not — `dir:` only sets the working directory, so writes are scoped by
> *path-patterned permissions* instead:
>
> ```yaml
> --allowedTools  Bash(go test:*) Bash(go build:*) Bash(go vet:*)
>                 Edit(./**) Write(./**)
> ```
>
> Without those patterns it will write anywhere it likes — verified: it created a
> file in `/tmp` without objection. With them, the same request is refused. Its
> shell is narrower than Codex's in exchange: it can run the project's build and
> test commands and nothing else.
>
> The cost either way is that anyone who can reach that room can cause writes to
> that directory. Point it at a checkout you can throw away.

Two things change once an agent in the room can read a repository. Cancelling a
turn now has to reach a *process*, so the shim kills the whole group rather than
the one command — a coding agent spawns builds and test runners, and killing
only the parent leaves those burning CPU on a turn nobody is waiting for. And a
turn is a whole agentic loop rather than one model call, which is what makes
**who answers** stop being an optimisation: 133 model calls versus 35 is a nice
saving when a call is cheap and a different product when it is a three-minute
coding turn.

**Codex is the one that proves the shim is not a Claude Code adapter.** The two
CLIs disagree on nearly everything: Claude Code accepts a session id you name,
Codex mints its own and announces it as `thread_id`; resuming is a flag on one
and a subcommand on the other, and Codex's `resume` rejects the sandbox flag its
own `exec` requires, so it has to arrive as a config override. That is why
`first` and `resume` are complete command lines rather than a base plus extras —
a scheme clever enough to append flags would have broken on the second agent,
not the third.

None of it reaches the connector, and neither CLI needed a spec change.

---

## Commands

```bash
go install ./cmd/oryxa      # or: go build -o oryxa ./cmd/oryxa
```

**Server**

```
oryxa serve                 run the API and viewer
oryxa launch window         run and open the viewer in a browser
oryxa-shim -agents FILE     serve command-line agents over HTTP
```

**Connectors** — read files, so they work before anything is running

```
oryxa agents                list configured connectors
oryxa which <agent>         where a connector points, and which file said so
oryxa check <agent>         probe an agent with a real turn
oryxa wake "a message"      who would answer it, and why
```

**Rooms** — talk to a running server

```
oryxa open <agent>...       start a session
oryxa send <session> TEXT   say something   (-as name, -f to follow)
oryxa tail <session>        follow the live stream
oryxa replay <session>      print the history
oryxa sessions              list sessions
oryxa context <session>     read or write shared context
```

Every command takes `-json` for scripting. Room commands take `-server` and
`-token`, falling back to `ORYXA_URL` and `ORYXA_TOKEN`. Connector commands take
`-connectors`, falling back to `ORYXA_CONNECTORS`.

A whole session from the shell:

```bash
SID=$(oryxa open langgraph crewai -json | jq -r .id)
oryxa send $SID "should we use event sourcing?" -f
oryxa context $SID -append decisions -value "revisit after the spike"
oryxa replay $SID
```

`oryxa which` exists because `base` is templated — the same connector resolves
differently on a machine and in a container, and *"it works in my shell but not
in the server"* is otherwise a confusing afternoon:

```
  langgraph

  file         connectors/langgraph.yaml
  base         http://{{env.ORYXA_AGENT_HOST}}:2024
  resolves to  http://localhost:2024
  turn         POST /threads/{{handle}}/runs/stream
  open         POST /threads

  ORYXA_AGENT_HOST=localhost — a containerised server resolves this differently
```

## Running it for real

Two flags separate a demo from something you can leave running:

```bash
oryxa serve \
  -db    'postgres://user:pass@host:5432/oryxa?sslmode=disable' \
  -token "$(openssl rand -hex 32)"
```

**`-db`** makes the event log durable. Sessions are a fold over that log, so a
restart replays them — history, agents and all — rather than losing them.
Without it the log is in-memory and the server says so at startup.

```
  ├─ store       postgres — postgres://oryxa:****@127.0.0.1:5432/oryxa
  ├─ auth        shared token
     recovered 3 session(s) from the log
```

A turn that was *running* when the process died is marked interrupted rather than
re-run: the agent may well have finished it, and doing the work twice is worse
than saying the outcome is unknown. Turns that were only queued are re-queued.

**Rooms carry their own secret.** Creating one returns it, once:

```bash
oryxa open claude-code codex
#   s_9a2d0e49
#   secret  72177eac70…
#   someone else joins with: oryxa tail s_9a2d0e49 -secret 72177eac70…
```

The API token says you may talk to this server; the room secret says which rooms
are yours. Send it as `X-Oryxa-Session`, or let the CLI remember it — rooms
opened on this machine are written to `~/.config/oryxa/rooms.json` with mode
0600, so `send` and `tail` find it without being told.

Only a hash is kept, so it cannot be reissued and a stolen backup carries no
keys. A wrong secret and a room that does not exist answer identically, because
telling them apart would say which rooms are real.

**`-token`** guards the API. Send it as `Authorization: Bearer <token>`, or sign
in through the viewer — which exchanges it for an HttpOnly cookie, because
`EventSource` cannot set headers and the live stream needs to authenticate too.
Without it, anyone who can reach the port has full access, and the server says
that at startup as well.

**`-trust-header`** decides who is acting. Oryxa deliberately has no accounts —
your deployment already has identity, and duplicating it here would be the same
mistake as duplicating orchestration. Point it at the header your proxy already
sets and the log records people instead of claims:

```bash
oryxa serve -trust-header X-Forwarded-User   # oauth2-proxy, Pomerium,
                                             # Cloudflare Access, ALB, Istio…
```

The body cannot override it, and a request that skipped the proxy is **refused**
rather than treated as anonymous — treating it as anonymous would accept exactly
what the proxy exists to prevent.

> Only safe when nothing can reach the port except that proxy. Bind to localhost
> or a private network; a header is spoofable by anyone who can connect directly.

Unset, authors stay self-declared, and the startup banner says so:
`identity  self-declared — the log records claims, not people`.

All three default to off so the two-minute quickstart stays two minutes. None
should be off anywhere else.

**Turns are bounded** — 30 a minute per room and 120 across the server by
default, `0` for unlimited. A turn is an agent doing work, so this is the budget
that costs money rather than a request counter: one message can wake seven
agents and cost seven, or wake nobody and cost nothing. The wake ladder shows up
in the bill.

```bash
oryxa serve -room-turns-per-min 30 -turns-per-min 120
```

**`-admin-token`** guards adding and removing agents, sent as `X-Oryxa-Admin`.
Reading the registry and `check` stay open, so the viewer keeps working. And
removing an agent an open room holds is refused with the rooms named — it would
leave them with a lane that can never run a turn — unless you mean it:
`?force=true`.

**Agents registered over the API may only reach public addresses.** `base` is a
URL this server fetches, so without a line there, `POST /v1/agents` reads any
internal service — and on a cloud host, the metadata endpoint holding its
credentials. Connectors in `connectors/` are unaffected and may point anywhere,
because a file on disk is configuration and pointing at localhost is what every
connector here does. `-allow-private-agents` restores the old behaviour when you
register agents on a private network deliberately.

## Shared context

A room has shared state alongside its transcript — notes, decisions, working
values that everyone can read and write. Like sessions, it is a fold over the
event log, so it survives a restart and every change is attributed.

Two kinds, because most shared state is one of two shapes:

```bash
# append — add-only. Conflicts are impossible by construction, so this is
# the default: findings and notes are what most shared content actually is.
curl -X POST .../context/findings -d '{"append":"postgres handles concurrent writers"}'

# value — a single mutable value under optimistic concurrency.
curl -X POST .../context/decision -d '{"value":"start with sqlite"}'
curl -X POST .../context/decision -H 'If-Match: 4' -d '{"value":"switch to postgres"}'
```

A stale write is **refused with what is current**, so the caller can merge
instead of guessing:

```json
409  {"error":"stale write to \"decision\": current version is 4",
      "current":"start with sqlite","version":4,"by":"carol"}
```

Loud conflicts beat clever merges. A silent overwrite leaves state that is
syntactically fine and semantically wrong, and surfaces much later as an agent
behaving oddly. Rejections are recorded as `conflict.rejected` too — two people
fighting over a key is a signal worth seeing, not something to bury in retry
logic.

Entries can be **pinned**: the small curated set meant to be put in front of an
agent without swamping it.

### Agents read it and write it

Two lines in a connector, and the agent joins the room's thinking. It does not
need tool calling, a prompt change, or any awareness that Oryxa exists.

```yaml
turn:
  body:
    message: "{{context}}\n\n{{input}}"   # reading: splice it into the prompt

context:                                  # writing: what this agent leaves behind
  - key: findings
    from: $text
```

Now a researcher's findings are in front of the writer on its next turn, and
neither knows the other is there. Rules take `kind: value` for state with one
current answer, `when:` to gate streaming chunks, and `pin: true` for the
curated set — see [docs/integrating.md](docs/integrating.md#shared-context).

Context is snapshotted when a turn starts, so an agent finishing mid-flight
never rewrites the question another was asked. A failed or cancelled turn writes
nothing: half an answer recorded as a finding is worse than no finding.

### A long room stops forgetting

A list is bounded where it is **rendered**, not where it is stored: twenty items
reach a prompt and the log keeps everything. Left there, a long room forgets
quietly — an agent answering from a partial room sounds exactly as confident as
one answering from a whole one.

Point Oryxa at a summariser and what fell off gets said instead of counted:

```bash
oryxa serve -summariser room-summariser
```

```
findings:
- (30 earlier items, summarised) Checkout returned 503s from 14:02, traced to a
  pool capped at 10 against 40 workers. A rollback was attempted twice.
- the error rate is back under 1 percent
```

The summariser is an ordinary connector, so this is a slot rather than a model
dependency — unconfigured, the count-only marker stays and Oryxa still runs with
no API key. A summary is written **once** and replayed as data: it is a model's
output, and recomputing it would give a different room after every restart.

## Docker

```bash
docker build -t oryxa .
docker run -p 8080:8080 -v ./connectors:/connectors oryxa
```

15MB, `scratch`, no cgo — the viewer is embedded, so there is nothing to serve
alongside it. SIGTERM drains rather than killing in-flight turns.

## Endpoints

```
GET    /                                 the viewer
POST   /v1/agents                        register (JSON or YAML)
GET    /v1/agents                        list
POST   /v1/agents/{name}/check           probe with a real turn

POST   /v1/sessions                      {agent} -> session
GET    /v1/sessions/{id}                 state, queue, history
POST   /v1/sessions/{id}/input           {text, author, to?} — returns who it woke
DELETE /v1/sessions/{id}/input/{tid}     withdraw a queued turn
POST   /v1/sessions/{id}/cancel          stop the running turn
POST   /v1/sessions/{id}/close

GET    /v1/sessions/{id}/context         shared state
POST   /v1/sessions/{id}/context/{key}   {append} or {value} (If-Match: version)
POST   /v1/sessions/{id}/context/{key}/pin

`POST /v1/agents` takes the same connector as JSON, so an agent can be added at
runtime with no restart and no filesystem access — which is how a platform whose
users build agents in a UI would onboard one. Registrations are durable: they go
to the event log and are restored on start, attributed to whoever made them.
`POST /v1/agents/{name}/check` runs a real turn and returns structured
diagnostics, which is the Test Connection button already written.

GET    /v1/sessions/{id}/stream?since=   SSE, resumable from any point
GET    /v1/sessions/{id}/events?since=   raw log
```

---

## How it works

```
  alice   bob   carol          many people, one session
    └──────┼──────┘
           ▼
    ┌─────────────┐
    │   Oryxa     │  owns: input queue, event log, presence, streaming out
    └──────┬──────┘
           │ connector (declarative HTTP)
           ▼
    ┌─────────────┐
    │ your agent  │  keeps: prompts, tools, memory, multi-agent orchestration
    └─────────────┘
```

**One turn at a time, per agent.** Not a preference — an agent's own conversation
is sequential, so overlapping its turns would misrepresent what's underneath.
Input arriving mid-turn queues in that agent's lane. Different agents have
different conversations and run in parallel: five live frameworks answering one
question took 4.9s of wall clock for 15.6s of work.

**The log is the source of truth.** Sessions are a fold over it. That's why late
join, replay and audit are one mechanism rather than three features.

**We never look inside the agent.** Text is text; everything else it emits is
passed through as opaque activity. Parsing a framework's event schema would
couple us to its release cycle.

---

## Layout

```
cmd/oryxa/          server
cmd/oryxa-shim/     HTTP in front of agents that only speak stdin and stdout
cmd/mockagent/      stand-in agent for testing
internal/connector/ spec, templating, selectors, HTTP executor, check
internal/session/   the room: membership, queue, turn loop
internal/events/    append-only log
internal/api/       HTTP surface
connectors/         connector files, loaded at startup
shim.yaml           what the shim may run — never settable over HTTP
```

```bash
go test ./...          # unit + end-to-end
go test -race ./...
```

## Not built yet

**Read scoping** is the one that blocks real use: one token opens every session
and there is no participant concept anywhere, which makes this laptop-safe
rather than deployable.

Behind it — agents write to shared context when a turn finishes, not during one;
nothing summarises a room's older findings once the render bound stops showing
them; no presence (who's here, who's typing); and events carry neither a hash
chain nor model token usage.

[`PLAN.md`](PLAN.md) is the source of truth and carries the same list with the
reasoning for each; design docs are in `design/`.

## Contributing

Contributions are genuinely welcome, and small ones count. The highest-value
thing is a connector for a framework we haven't tested — a single YAML file, no
Go needed. [CONTRIBUTING.md](CONTRIBUTING.md) has the three-step version.

If you tried Oryxa and got stuck, an issue saying where you got lost is worth
real work to us: it tells us exactly where the docs are wrong.

## Building on it

**[`openapi.yaml`](openapi.yaml)** is the contract — every route, every shape,
written against the handlers rather than from memory. A test compares it to the
registered routes in both directions, so it cannot drift into being almost right.

**A Go client**, if you want one:

```go
import "github.com/oryxa/oryxa/client"

c := client.New("http://localhost:8080")
room, _ := c.Open(ctx, "langgraph", "adk")
in, _ := c.Say(ctx, room.ID, "alice", "what should we do about the budget")
// in.Wake and in.Why record who it reached, and on what grounds
c.Stream(ctx, room.ID, 0, func(ev client.Event) bool { … })
```

Nothing requires it. The HTTP API is the contract and any language can call it;
this exists so a Go service does not write the same twenty request shapes again.

### The contract, and a client

[`openapi.yaml`](openapi.yaml) is the HTTP contract — every route, every shape,
written against the handlers rather than from memory. A test fails if the code
and the file disagree, in either direction.

For Go, [`client/`](client/) wraps it:

```go
c := client.New("http://localhost:8080")
room, _ := c.Open(ctx, "researcher", "critic")

in, _ := c.Say(ctx, room.ID, "priya", "what should we do about the budget")
// in.Wake = ["researcher"], in.Why = "interest: budget"

c.Stream(ctx, room.ID, 0, func(ev client.Event) bool {
    fmt.Println(ev.Kind, ev.Actor)
    return true
})
```

It is the only non-internal Go package, and the reason is narrow: it is a thin
wrapper over `/v1`, so it is stable exactly as far as `/v1` is.

## Stability

`/v1` is stable. The connector spec is stable and additive — new fields, never
changed meanings — so a connector written today keeps working.

`client` is the one exported Go package, and only because it is a thin wrapper
over `/v1`: it is stable exactly as far as `/v1` is. Everything else stays
`internal/` until 1.0 — those interfaces are young, and exporting one is a
promise that cannot be withdrawn.

## Licence

Copyright 2026 Oryxa. Licensed under the [Apache License 2.0](LICENSE).
