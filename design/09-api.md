# Oryxa — API & Connector Spec

**Date:** 2026-08-05
**Status:** spec for M1

Two surfaces:
- **§1 Connector spec** — how Oryxa is told to call *your* agent. Declarative, so no framework
  is baked into the core.
- **§2 Oryxa endpoints** — what Oryxa exposes.

---

## 1. Connector spec

### 1.1 Neutrality by construction

The core knows three things about an agent, and none of them are framework words:

| Concept | Meaning |
|---|---|
| **target** | an HTTP endpoint |
| **conversation** | optional continuity across turns |
| **turn** | input in, parts out |

"Session", "thread", "assistant", "app", "run" are all *their* vocabulary. None appear in the
core. A connector is a **description of HTTP calls** — the same spec must be able to express
ADK, LangGraph, and a plain Flask endpoint without favouring any of them. If a field only makes
sense for one framework, it doesn't belong in the spec.

### 1.2 Shape

```yaml
name: my-agent
base: http://localhost:8000
headers:                       # optional, applied to every call
  Authorization: "Bearer {{env.AGENT_TOKEN}}"

vars:                          # connector-specific constants
  app: research_agent
  user: oryxa

capabilities: [stream, multi_turn, callback]

open:                          # optional — run once per Oryxa session
  method: POST
  path: /apps/{{app}}/users/{{user}}/sessions/{{conversation}}
  body: {}
  capture:
    handle: $.id               # available as {{handle}} afterwards

turn:                          # required
  method: POST
  path: /run_sse
  body:
    app_name: "{{app}}"
    session_id: "{{handle}}"
    new_message:
      role: user
      parts: [{ text: "{{input}}" }]
  response:
    format: sse                # sse | ndjson | json
    text:                      # tried in order; first match wins
      - $.content.parts[*].text
      - $.delta
    done: $.turn_complete      # optional
    error: $.error.message     # optional
```

### 1.3 Variables

| Variable | From |
|---|---|
| `{{input}}` | the turn's composed input |
| `{{conversation}}` | Oryxa session id |
| `{{handle}}` | captured by `open` (falls back to `conversation`) |
| `{{callback_url}}`, `{{callback_token}}` | for the collaboration tool |
| `{{vars.*}}` | connector config |
| `{{env.*}}` | server environment |

### 1.4 Path selectors

Deliberately small — dot notation, `[*]`, `[n]`, and `[key]` / `[!key]` predicates. Enough for
real response shapes without pulling in a full JSONPath engine and its edge cases.

```
$.content.parts[*].text            $.choices[0].delta.content     $.output
$.content.parts[!thought].text     reasoning models: skip the thinking
```

No selector, or no match → the raw chunk is treated as text. **Being wrong in the direction of
"show the user something" beats silently emitting nothing**, which is the failure mode that
wastes an afternoon.

### 1.5 What reasoning models forced into the spec

Both of these came from running real models, not from design. Neither is optional now.

**`[!key]` predicates.** Reasoning models interleave their scratchpad with the answer in one
array, flagged per element (ADK sets `thought: true`). Without a predicate the connector
concatenates the model's thinking into the reply — and it looks like a plausible answer, which
is the worst kind of wrong.

**`when:` gates.** Streaming APIs commonly emit incremental deltas *and* a final aggregated
message. Consume both and the answer appears twice, verbatim. `when: $.partial` keeps the
deltas only.

Gated-out chunks are emitted as `activity`, never dropped — a mis-set `when` must be visible in
the log rather than hidden behind an empty reply.

---

## 2. Oryxa endpoints

### 2.1 Agents

```
POST   /v1/agents                register or replace a connector
GET    /v1/agents                list
GET    /v1/agents/{name}         fetch
DELETE /v1/agents/{name}
POST   /v1/agents/{name}/check   probe: reachability, open, a real turn
```

`check` is the integration workhorse — it answers *is it me or is it them* without starting a
session:

```json
{
  "reachable": true,
  "open":      {"ok": true, "handle": "s-123"},
  "turn":      {"ok": true, "ms": 1240, "parts": 14, "text_len": 380},
  "streaming": true,
  "capabilities": ["stream", "multi_turn"],
  "warnings":  ["no text selector matched; fell back to raw chunks"]
}
```

### 2.2 Sessions

```
POST   /v1/sessions                     {agent} → session
GET    /v1/sessions                     list
GET    /v1/sessions/{id}                state, people, queue, current turn
POST   /v1/sessions/{id}/input          {text, author} → turn (queues if busy)
DELETE /v1/sessions/{id}/input/{tid}    withdraw a queued turn
POST   /v1/sessions/{id}/cancel         stop the running turn
POST   /v1/sessions/{id}/close
```

### 2.3 Stream & events

```
GET /v1/sessions/{id}/stream?since={seq}    SSE — live, resumable from any point
GET /v1/sessions/{id}/events?since={seq}    raw log
```

`since` is what makes late joins, reconnects, and replay the same mechanism instead of three
features.

### 2.4 Context

```
GET  /v1/sessions/{id}/context
POST /v1/sessions/{id}/context/{key}        append | set (If-Match: {version})
POST /v1/sessions/{id}/context/{key}/pin    pin | unpin
```

### 2.5 Callbacks — the agent talking back

Authenticated by the per-turn `{{callback_token}}`, so the session is implied and an agent can
never address a session it wasn't invoked for.

```
POST /v1/callback/post          {text}          → visible to everyone, live
POST /v1/callback/ask           {question}      → blocks; returns the room's answer
GET  /v1/callback/context
POST /v1/callback/context/{key}
```

---

## 3. Events

```
session.created   session.closed
input.submitted   input.withdrawn
turn.started      turn.finished    turn.failed    turn.cancelled
output.part
context.appended  context.set      context.pinned
ask.raised        ask.answered     ask.timed_out
```

Append-only, ordered, attributed. Sessions and context are folds over it. Never coalesce, never
dedupe.

---

## 4. M1 — what gets built now

Enough to register a real framework and run a real turn through it.

**In:** connector spec + registry · HTTP executor (template, call, SSE/NDJSON/JSON, selectors) ·
`check` · sessions · input queue · turn loop · event log · SSE stream.

**Next, not now:** context, callbacks, cancel, multi-user presence, persistence.

**Store:** in-memory behind an interface. SQLite drops in without touching handlers — and
keeping M1 in-memory means the first thing we test is the integration, not the schema.

---

## 5. Open

| # | Question | Lean |
|---|---|---|
| 1 | Non-HTTP transports (stdio, gRPC) | HTTP only for now; `transport:` field reserved |
| 2 | Retry / timeout per connector | per-connector timeout in M1; retries later — retrying a non-idempotent turn is its own problem |
| 3 | Does `open` re-run if the agent restarts and loses the handle? | yes, on a 404 from `turn` — one retry, then fail loudly |
| 4 | Auth on Oryxa's own endpoints | none in M1 (local dev), bearer tokens before anything real |
