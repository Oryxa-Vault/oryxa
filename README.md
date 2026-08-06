# Oryxa

**Agent frameworks are single-user. Oryxa makes them multi-user.**

LangGraph, CrewAI and ADK already do multi-agent well. None of them let several
people share one agent session — watch it live, add input, hand off, pick up
where someone left off. That's the job.

> To the framework, Oryxa is one user. To your team, it's a shared room.

Status: **M1** — connectors, rooms (many people, many agents), event log, live stream, viewer.

---

## Try it in two minutes

```bash
go build -o oryxa ./cmd/oryxa
go run ./cmd/mockagent &          # a stand-in agent on :9000
./oryxa launch window              # server + viewer, opens a browser
```

Pick an agent in the sidebar — clicking it runs a real probe turn and shows what
came back. Then **+ new session**, and talk to it.

**To see it as two people:** open the viewer in a second tab and change the name
in the composer. Both tabs are in the same room, watching the same agent.

**Several agents in one room:** click more than one agent before creating the
session. Input fans out to each — one question, one answer per framework. Each
gets its own lane: its own queue, its own goroutine, its own conversation with
its own framework. They answer **in parallel**, so a room costs its slowest
agent rather than the sum of all of them. One agent failing does not stop the
others.

```bash
curl -X POST localhost:8080/v1/sessions \
  -d '{"agents":["langgraph","adk","pydanticai","crewai"]}'
```

```bash
oryxa check langgraph          # probe a connector, no server needed
oryxa serve                    # API + viewer, no browser
```

Bob's input queues if Alice's turn is still running, and runs next. Anyone
joining later replays the whole history and then follows live — `?since=` makes
late join, reconnect and replay the same code path.

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

Five genuinely different surfaces, one core, one YAML file each.
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

---

## Commands

```
oryxa serve            API + viewer on one port
oryxa launch window    the same, and opens a browser
oryxa check <agent>    probe a connector with a real turn; no server needed
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

> Today context is people-facing — agents can't read or write it until the
> collaboration tool (`post` / `ask` / `read` / `write`) lands.

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
POST   /v1/sessions/{id}/input           {text, author} — queues if busy
DELETE /v1/sessions/{id}/input/{tid}     withdraw a queued turn
POST   /v1/sessions/{id}/cancel          stop the running turn
POST   /v1/sessions/{id}/close

GET    /v1/sessions/{id}/context         shared state
POST   /v1/sessions/{id}/context/{key}   {append} or {value} (If-Match: version)
POST   /v1/sessions/{id}/context/{key}/pin

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
cmd/mockagent/      stand-in agent for testing
internal/connector/ spec, templating, selectors, HTTP executor, check
internal/session/   the room: membership, queue, turn loop
internal/events/    append-only log
internal/api/       HTTP surface
connectors/         connector files, loaded at startup
```

```bash
go test ./...          # unit + end-to-end
go test -race ./...
```

## Not built yet

The collaboration tool (`post` / `ask` / `read` / `write`) — so agents can use
shared context, not just people. Presence (who's here, who's typing). Event hash
chaining and usage accounting.

Design docs are in `design/`; [`PLAN.md`](PLAN.md) is the source of truth.

Apache 2.0.
