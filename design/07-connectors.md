# Oryxa — Connectors

**Date:** 2026-08-05
**Status:** draft for review

How frameworks plug in without either side getting locked in.

---

## 1. The contract

Oryxa's core never knows which framework it's talking to. Everything framework-specific sits
behind four operations:

```
Connector
  ensure(session)      -> handle      # map an Oryxa session to their thread/session
  send(handle, input)  -> stream      # one turn in, parts out
  cancel(handle)                      # optional
  probe()              -> caps        # what this connector supports
```

That's the whole surface. If a framework needs a fifth operation, it's doing something we
decided not to do.

---

## 2. What crosses the boundary

Deliberately thin, because every type we adopt from a framework is a type we're stuck with.

**In:** text, plus optional structured parts.
**Out:** a stream of:

| Part | Meaning |
|---|---|
| `text` | a delta of the answer |
| `activity` | the agent did something — **opaque, labelled, never parsed** |
| `done` | turn finished |
| `error` | turn failed |

`activity` is the important one. Frameworks emit rich internal events — ADK event types,
LangGraph node transitions, tool calls. **We pass them through as labelled opaque blobs and
never interpret them.** The moment we start parsing ADK's event schema we're coupled to ADK's
release cycle, and we've also broken "we never look inside the agent."

Text is text. Everything else is a blob with a name on it.

---

## 3. Connectors are configuration, not code

Both ADK and LangGraph are plain HTTP. So a connector is a **description**, not a plugin:

```yaml
kind: adk
capabilities: [stream, multi_turn, callback]
ensure:
  POST /apps/{app}/users/oryxa/sessions/{session}
send:
  POST /run_sse
  body:
    app_name:    "{app}"
    user_id:     "oryxa"
    session_id:  "{session}"
    new_message: "{input}"
  stream: sse
```

This matters more than it looks:

- **Adding a framework is a YAML file**, not a Go release.
- **When a framework ships a breaking change, users can fix it themselves** — same day, without
  waiting for us. Connectors living in the core binary would put every framework's release
  cycle on our critical path. That's the treadmill that kills integration-heavy projects.
- Connectors are shareable. A community connector is a file in a repo.

**Escape hatch:** frameworks needing real logic — odd auth, pagination, non-standard streaming
— get a code connector implementing §1. Declarative covers the common case; code covers the
rest. Neither is privileged.

---

## 4. Capabilities, not assumptions

Connectors declare what they support. The core adapts rather than requiring a
lowest-common-denominator.

| Capability | If absent |
|---|---|
| `stream` | turn is delivered whole when it completes |
| `multi_turn` | each turn starts fresh; Oryxa carries context instead |
| `callback` | agent can't `ask`; `post` still works one-way |
| `cancel` | a running turn plays out; users just stop waiting |

Nothing is required except `send`. A framework with only `send` still works — it's just a
one-shot, one-way session. That's a real degradation and it should be **visible in the UI**, not
silently absent.

---

## 5. Session mapping

We ride their session model rather than replacing it, so the agent keeps its own memory and
state exactly as today. But the mapping lives **in the connector**, never in the core.

| Oryxa | ADK | LangGraph | generic |
|---|---|---|---|
| session | session | thread | whatever `ensure` returns |
| agent | app | assistant | endpoint |
| turn | run | run | request |

ADK's route carries a `user_id` because ADK assumes one user per session. **Oryxa takes that
slot** — one framework-facing user, many real people behind it. That's the seam the product
sits in.

The core only ever sees an opaque `handle`. It does not know ADK has users or LangGraph has
assistants, and it must never learn.

---

## 6. The callback path

When the agent uses the collaboration tool (`post` / `ask` / `read` / `write`), it calls
**Oryxa's own API** — framework-neutral by construction, since it's our surface, not theirs.

Binding: Oryxa mints a short-lived token per turn and passes it through the connector's metadata
slot. ADK has session state; LangGraph has thread config. A framework with no slot declares
`callback: false` and gets `post`-only via its output stream.

Degrade, declare it, show it. Never guess.

---

## 7. Anti-lock-in rules

Both directions, because both matter.

**They don't get locked into us**
- Their agent runs standalone, unchanged, with Oryxa removed. Integration is additive, always.
- We use only public HTTP APIs and public entrypoints. No monkeypatching, no private imports —
  those break on every minor release and they poison trust when they do.
- The collaboration tool is optional. No Oryxa, no tool, still a working agent.

**We don't get locked into them**
- Core sees `Connector`, never a framework name. A `grep` for "adk" or "langgraph" outside
  `connectors/` should return nothing — that's the test, and it's worth enforcing in CI.
- No framework type crosses the boundary (§2).
- Connectors version and ship independently of the core (§3).

---

## 8. `oryxa check`

Integrations decay silently. One command, run against a configured agent:

```
$ oryxa check researcher
  reachable            ok    http://localhost:8000
  ensure session       ok    created adk session
  probe turn           ok    1.2s, 14 parts
  streaming            ok
  callback             ok    token accepted
  cancel               n/a   not declared
```

This is what makes "integrates easily" hold over time instead of being true once at launch. It
also turns the most common support question — *is it me or is it them* — into a command.

---

## 9. Open

| # | Question | Lean |
|---|---|---|
| 1 | Declarative connector format — bespoke YAML, or lean on an existing spec? | bespoke and small; a full API-description spec is far more than four operations need |
| 2 | Where do community connectors live? | one repo, one file each, versioned by framework version |
| 3 | Streaming shapes — SSE, chunked JSON, websocket | SSE first (both named frameworks use it), pluggable per connector |
| 4 | Does `ensure` create their session, or require it exists? | create; failing on a missing session pushes setup back onto the user |
| 5 | Connector for plain HTTP agents | yes — `POST {url}` with `{input}` → `{output}`. The zero-framework path, and the fallback when nothing else fits |
