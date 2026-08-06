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
session. Input fans out to each — one question, one answer per framework, still
one turn at a time. Each keeps its own conversation with its own framework, and
one agent failing does not stop the others.

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

Both default to off so the two-minute quickstart stays two minutes. Neither
should be off anywhere else.

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

**One turn at a time.** Not a preference — the agent's own conversation is
serial, so a session running turns concurrently would misrepresent what's
underneath. Input arriving mid-turn queues.

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

Shared context (`log` / `state` regions, OCC), the collaboration tool
(`post` / `ask` / `read` / `write`), presence, per-person identity (the token is
shared, so the log records who *said* they were alice, not who they are).

Design docs are in `design/`; [`PLAN.md`](PLAN.md) is the source of truth.

Apache 2.0.
