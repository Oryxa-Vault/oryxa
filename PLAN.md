# Oryxa — Plan

**Status:** current. What is built, what isn't, and what the lines are.
For using it, start with [README.md](README.md) and
[docs/integrating.md](docs/integrating.md).

---

## 1. The job

> **Agent frameworks are single-user. Oryxa makes them multi-user.**

LangGraph, CrewAI and ADK already do multi-agent well. We don't touch that. What
none of them do is let **several people share one agent session** — watch it
live, add input, hand off, pick up where someone left off.

> **To the framework, Oryxa is one user. To your team, it's a shared room.**

| Theirs | Ours |
|---|---|
| agent logic, prompts, tools | the session |
| multi-agent orchestration | the people in it |
| memory, state, model calls | the event log and what's derived from it |

---

## 2. How it works

Control is reversed: the framework doesn't own the conversation, Oryxa does.
People talk to a room; Oryxa drives the agents in it.

```
   alice   bob   carol           many people
     └──────┼──────┘
            ▼
   ┌────────────────┐
   │ Oryxa session  │  input queue · event log · live stream
   └───┬────────┬───┘
       │        │  one turn at a time, per agent
       ▼        ▼
   ┌───────┐ ┌───────┐
   │ agent │ │ agent │  each keeps its own conversation, unchanged
   └───────┘ └───────┘
```

**Connecting an agent is one YAML file.** A connector describes HTTP calls —
optionally *open* a conversation, then *run a turn* — and where the answer text
sits. Nothing in the core knows any framework's name; CI enforces that.

Full reference: [docs/integrating.md](docs/integrating.md).

---

## 3. What is built

**Connectors.** Declarative HTTP specs with variables, path selectors, `when:`
gates and declared capabilities. `oryxa check` probes one with a real turn and
warns about the ways a connector can pass while being quietly wrong.

**Sessions.** Many people, one or more agents. Input queues and fans out — one
turn per agent — and runs one at a time, because each agent's own conversation is
serial. Input carries who sent it. Queued turns can be withdrawn.

**Events.** Append-only, ordered, attributed. Sessions are a fold over the log,
which is why late join, replay and audit are one mechanism rather than three
features. `?since=` serves all of them.

**Viewer.** Embedded in the binary. Live transcript, agent health, a raw view
showing every chunk exactly as the agent sent it.

**CLI.** `serve`, `launch window`, `check`.

### Verified against real servers and a real model

| Framework | Shape |
|---|---|
| LangGraph | `langgraph dev` — per-thread URL, SSE token deltas |
| Google ADK | `adk api_server` — ids in body, reasoning filter, `when:` gate |
| Pydantic AI | FastAPI wrapper, SSE |
| CrewAI | FastAPI wrapper, non-streaming, multi-agent inside one turn |
| AG-UI | a protocol, not a framework — typed events, tool calls |

Also verified: token-level streaming, multi-turn continuity across several
people, a tool-using agent (six tool calls in one turn, none leaking into the
answer), and one agent failing without taking the room down.

---

## 4. What is not built

| | |
|---|---|
| **Shared context** | `log` / `state` regions with optimistic concurrency — designed in [08-sessions-context.md](design/08-sessions-context.md), not implemented |
| **Collaboration tool** | `post` / `ask` / `read` / `write` — the agent talking back to the room mid-turn |
| **Presence** | who is here, who is typing |
| **Persistence** | the event store is in-memory behind an interface; SQLite drops in without touching handlers |
| **Auth** | none; fine on localhost, nowhere else |
| **Cancel** | in the spec and the session API, no connector declares it and it is unexercised end to end |

Nothing above is blocked. Persistence and auth are what stand between this and
running somewhere real.

---

## 5. The lines

Not style preferences — these are what make "works with any framework" true
rather than aspirational.

1. **Multi-user, not multi-agent.** Their orchestration stays theirs.
2. **We never look inside the agent.** Text is text; everything else is recorded
   whole as opaque activity and never interpreted.
3. **We never touch prompts.**
4. **Events are the truth.** Anything derived is derived; if a feature wants to
   write to a projection, the answer is an event.
5. **The log is append-only.** Never coalesce, never dedupe.
6. **Capabilities are declared, never assumed.** No fake streaming, no dead stop
   button.

---

## 6. Settled by testing

Questions this project actually answered, rather than guessed:

- **Queue or interrupt when someone sends mid-turn?** Queue — the agent's own
  conversation is serial, so interrupting isn't available to offer.
- **Does riding the framework's session model work?** Yes. Three people's turns
  accumulate into one coherent conversation the framework holds itself.
- **Do tool calls need special handling?** No. Treating everything that isn't
  text as opaque was already right; six real tool calls needed zero spec change.
- **Does a session survive its agent restarting?** In both frameworks tested,
  yes — they persist. The re-open path is unimplemented and so far untriggered.
- **Does CrewAI bypass its configured LLM object?** Only with `planning=True`.
  The broad claim was wrong; `planning_llm` fixes it.

---

## 7. Next

1. **Persistence** — SQLite behind the existing `events.Store`.
2. **Shared context** — regions and OCC, per [08](design/08-sessions-context.md).
3. **Collaboration tool** — `post` and `ask` make the room conversational.
4. **Auth** — session-scoped tokens.

---

## 8. Docs

| | |
|---|---|
| [README.md](README.md) | what it is, quickstart |
| [docs/integrating.md](docs/integrating.md) | connecting an agent — recipes, reference, troubleshooting |
| [CONTRIBUTING.md](CONTRIBUTING.md) | how to help; connectors are the best contribution |
| [research/01-landscape.md](research/01-landscape.md) | why this layer is open — the evidence base |
| [design/](design/) | current design: domains, sessions & context, connectors, API |
| [design/archive/](design/archive/) | superseded, kept with the reasoning that killed each one |
