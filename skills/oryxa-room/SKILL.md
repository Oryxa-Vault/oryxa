---
name: oryxa-room
description: Drive an Oryxa room over its HTTP API — open a session with one or more agents, send input as a named person, follow the live stream, read and write shared context, and replay history. Use when scripting against a running Oryxa, building a client, debugging a session, or asking several agents the same question at once.
---

# Driving an Oryxa room

A room is a session: several people, one or more agents, one ordered history.
Everything below is plain HTTP against a running Oryxa (default
`http://localhost:8080`).

If a token is configured, send `Authorization: Bearer $ORYXA_TOKEN` on every
`/v1` request.

## Open a room

```bash
# one agent
curl -X POST $O/v1/sessions -d '{"agent":"my-agent"}'

# several — one question, one answer each, running in parallel
curl -X POST $O/v1/sessions -d '{"agents":["langgraph","crewai","adk"]}'
```

Returns `{"id":"s_...","agents":[...],"state":"idle"}`.

## Talk to it

```bash
curl -X POST $O/v1/sessions/$SID/input \
  -d '{"text":"what should we use for the event log?","author":"alice"}'
```

Returns `202` with a turn. Input **fans out to every agent** in the room, one
turn each, tagged with a shared `group`.

Turns are ordered per agent and parallel across agents. Input arriving while
that agent is busy queues; it does not interrupt.

`author` is what the caller claims. If the server runs with `-trust-header`, the
proxy's header wins and the body is ignored.

## Watch it

```bash
curl -N "$O/v1/sessions/$SID/stream?since=0"
```

SSE, replayed from `since` then following live. `since=0` gives the whole
history first — late join, reconnect and replay are the same call.

Each event is `{seq, session, ts, kind, actor, turn, data}`. The kinds:

```
session.created  session.opened   session.closed
input.submitted  input.withdrawn
turn.started     turn.finished    turn.failed     turn.cancelled
output.part                        # data.kind = text | activity | error
context.appended context.set       context.pinned  context.unpinned
conflict.rejected
```

**Reconstruct an answer** by concatenating `output.part` events where
`data.kind == "text"`, grouped by `turn`. Parts with `kind == "activity"` are
the agent's internals — tool calls, reasoning, state — recorded whole and
deliberately not interpreted.

Prefer `?since=` over polling. To poll anyway, `GET /v1/sessions/{id}` returns
state, queue, current turn and history.

## Shared context

```bash
C=$O/v1/sessions/$SID/context

curl $C                                                   # everything

# append — add-only, cannot conflict. Default for notes and findings.
curl -X POST $C/findings -d '{"append":"postgres handles concurrent writers"}'

# value — optimistic concurrency
curl -X POST $C/decision -d '{"value":"start with sqlite"}'
curl -X POST $C/decision -H 'If-Match: 4' -d '{"value":"switch to postgres"}'

curl -X POST $C/decision/pin -d '{"pinned":true}'
```

A stale value write returns `409` **carrying what is current**:

```json
{"error":"stale write to \"decision\": current version is 4",
 "current":"start with sqlite","version":4,"by":"carol"}
```

Merge from that rather than re-reading and guessing. Without `If-Match` a write
overwrites; supply it whenever you read-then-write, which is the case that
matters.

Kinds are fixed on first write — appending to a value, or setting an append
list, returns `409`.

## Managing turns

```bash
curl -X DELETE $O/v1/sessions/$SID/input/$TID   # withdraw a queued turn
curl -X POST   $O/v1/sessions/$SID/cancel       # stop running turns
curl -X POST   $O/v1/sessions/$SID/close
```

Cancel only works if the connector declares the `cancel` capability. Check
`GET /v1/agents` before offering it.

## Agents

```bash
curl $O/v1/agents
curl -X POST $O/v1/agents/my-agent/check -d '{"probe":"hello"}'
```

`check` runs a **real** turn. Use it before blaming a session for something the
connector is doing.

## Useful shapes

**Ask several agents one question and compare:**
open a session with `agents: [...]`, submit once, poll `GET /v1/sessions/{id}`
until `history` has one entry per agent, then read each `output` with its
`agent`.

**Rebuild a transcript offline:**
`GET /v1/sessions/{id}/events?since=0` and fold it. That is exactly what the
viewer does, and what the server does after a restart.

## What to expect

- **One turn at a time per agent, parallel across agents.** A room costs its
  slowest agent, not the sum.
- **One agent failing does not stop the others** — its turn is `failed`, the
  rest still answer.
- **Everything survives a restart** when the server runs with a database. A turn
  that was running when the process died comes back `failed` with
  "interrupted by restart; outcome unknown" — it is not silently re-run, because
  the agent may well have finished it.
