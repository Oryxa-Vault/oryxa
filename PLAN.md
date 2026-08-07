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

**Sessions.** Many people, one or more agents. Input fans out to one turn per
agent, each landing in that agent's own **lane** — its own queue, its own
goroutine, its own conversation handle.

The serialization requirement is per agent, not per session: an agent's
conversation is sequential, but two agents have two conversations and no reason
to wait on each other. So turns are strictly ordered within a lane and fully
parallel across lanes. Measured on five live frameworks answering one question:
15.6s of work in 4.9s wall clock, bounded by the slowest agent rather than the
sum of all five.

**Events.** Append-only, ordered, attributed. Sessions are a fold over the log,
which is why late join, replay and audit are one mechanism rather than three
features. `?since=` serves all of them.

**Shared context.** Append entries (conflict-free by construction) and value
entries under optimistic concurrency, both attributed and both a fold over the
same log — so they survive a restart with versions and pins intact. A stale
write is refused with what is current, and the rejection is itself an event.

Agents reach it declaratively, not through a tool they must call: `{{context}}`
renders the room into whatever a connector calls the prompt, and `context:` rules
extract part of a finished turn back into it. That choice is load-bearing — a
tool would work only on frameworks with tool calling, would need a prompt change
per agent, and would leave recording the result at the model's discretion. A
rule works everywhere and cannot be talked out of. Context is snapshotted at turn
start, so parallel lanes never rewrite each other's questions, and a failed turn
writes nothing.

Size is bounded where it is rendered, not where it is stored. An append entry
puts its newest 20 items in a prompt and tells the model when it left older ones
out; the store and the log keep everything. Trimming the fold would need its own
event to survive a restart, and a room whose history depended on when the server
last came up would not be a history. Every `turn.started` records the version,
size and elided count its agent was handed — the log already said what an agent
replied, and now says what it was asked from.

**Viewer.** Embedded in the binary. Live transcript, agent health, a raw view
showing every chunk exactly as the agent sent it.

**Persistence.** Postgres event log behind the same `events.Store` interface.
Restart replays sessions from it — history, agents and per-agent handles — because
a session was already a fold over its events. A turn that was *running* when the
process died is marked interrupted rather than re-run: the agent may have finished
it, and doing the work twice is worse than admitting the outcome is unknown.

**Identity.** Taken from a header set by whatever runs in front of Oryxa —
`-trust-header X-Forwarded-User` and friends. There are deliberately no accounts:
deployments already have identity, and building it here would repeat the
orchestration mistake. The body cannot override the header, and a request that
skipped the proxy is refused rather than treated as anonymous. Unset, authors are
self-declared and the banner says so.

**Auth.** One shared token, constant-time compared. `Authorization: Bearer` for
clients; the viewer exchanges it for an HttpOnly cookie because `EventSource`
cannot set headers and the stream needs authenticating too. Off by default, and
the server says so at startup.

**CLI.** `serve`, `launch window`, `check`. Graceful shutdown on SIGTERM, so a
restart drains rather than manufacturing turns whose outcome is unknown.
15MB scratch image.

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
| **Read scoping** | one token opens every session; there is no participant concept anywhere. This is what makes the framework laptop-safe rather than deployable, and it is now the largest gap. |
| **Mid-turn writes** | rules apply when a turn finishes. An agent that wants to publish a finding *while* still working would need a callback — `{{callback_url}}` exists in the template context but nothing populates it yet. |
| **Presence** | who is here, who is typing. Now load-bearing rather than cosmetic: owner precedence in §7.4 is built on it. |
| **Participants** | agents have no owners. Read scoping, owner-waking and directed output all wait on this one idea — see §7.2. |
| **Addressing** | every input wakes every agent; there is no way to speak to one. §7 replaces the fan-out rather than patching it. |
| **Context rollup** | the render bound keeps a list's newest 20 items and says how many it dropped. Nothing merges, deduplicates or summarises what falls off, so a long room's early findings simply stop reaching prompts. Summarising would have to be a logged event carrying its own output — a summary cannot be recomputed on replay, so recomputing it would give a different room after every restart. |
| **Hash chaining** | events are ordered and attributed but not tamper-evident. Chaining each event to its predecessor's hash would make the log verifiable rather than merely durable — worth having before anyone treats it as an audit record. |
| **Usage accounting** | `turn.started` records what the *room* cost a prompt in characters, which is a different thing from what the *model* charged. No event carries token counts, so cost per turn cannot be derived from the log. |

Read scoping is the one that blocks real use. Participants is the one the most
other things wait on — see §7.

Cancel is no longer unexercised: `TestCancelledTurnWritesNothing` drives it end
to end. What remains untested is the capability path, where the agent is told to
stop rather than the request being abandoned — no connector declares `cancel`.

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
- **How much of a room must serialize?** One agent, not the room. Serializing the
  whole session was an invented constraint that cost a 3.2x slowdown on five
  agents; lanes removed it without weakening the guarantee that matters.
- **Does riding the framework's session model work?** Yes. Three people's turns
  accumulate into one coherent conversation the framework holds itself.
- **Do tool calls need special handling?** No. Treating everything that isn't
  text as opaque was already right; six real tool calls needed zero spec change.
- **Does a session survive its agent restarting?** In both frameworks tested,
  yes — they persist. The re-open path is unimplemented and so far untriggered.
- **Does CrewAI bypass its configured LLM object?** Only with `planning=True`.
  The broad claim was wrong; `planning_llm` fixes it.
- **Should Oryxa own user identity?** No. It accepts identity, it does not
  establish it — the same line already drawn around orchestration and prompts.

---

## 7. Next: rooms with more than four people in them

Everything below follows from one observation, which is that Oryxa currently
conflates two different acts.

**Listening and speaking are not the same thing.** Today a turn is both: an agent
cannot be in a room without answering, so every input fans out to every agent.
That is survivable at four and absurd at thirty-six — six people with six agents
apiece produce 216 turns for one round of chat, each lane runs them serially, and
because people type faster than models answer, the backlog grows without bound.

The instinct to fix this by making agents faster or queues smarter is wrong. In
any conversation the listener is behind the speaker; that is not a defect, it is
what listening is. Nobody in a group chat is "behind" — they are reading. The
defect is that Oryxa gives no way to read.

### 7.1 Cursors instead of fan-out

Reception becomes free and universal; production becomes a decision.

Each participant keeps a **cursor** — where they last spoke from — instead of a
queue. `Submit` appends one event rather than building N turns. A participant
that does not speak does nothing at all, and carries no pending work. When it
does speak, its turn covers everything since its cursor.

| | today | with cursors |
|---|---|---|
| on submit | N turns into N lanes | one event |
| a silent agent | a queued turn it will run later | nothing |
| when it speaks | replays its backlog turn by turn | one turn, everything since its cursor |
| cost | messages × agents | wake decisions |

A backlog cannot accumulate because there is no per-agent work queue to hold one.
This also removes the need for a separate transcript binding: what a turn is
built *from* is the log between the cursor and now.

### 7.2 Participants

Agents belong to people. Missing entirely today — identity is a per-message
author string and nothing else.

This is one idea that three separate things are waiting on: read scoping (the
largest gap in §4), waking an agent because its *owner* was addressed, and
directed output below. Worth building once, deliberately, rather than three
times in three shapes.

### 7.3 The wake ladder

With reception free, one question remains, and it is the only judgment the
framework makes on its own behalf: **when this lands, who speaks?**

Cheapest rung first, because most cases never need the expensive one:

1. **Explicit** — `@legal-head`, or `to:` on the input
2. **Named** — the agent's name, or its owner's name, appears
3. **Declared interest** — the connector says what it engages on
4. **Semantic** — embed the message once, compare against precomputed agent
   descriptions; one call per round regardless of roster size
5. **Router** — a model chooses within the shortlist

Interests inherit, because org structure is the normal case:

```yaml
name: legal
kind: group
interests: [contract, liability, nda, compliance]

name: legal-head
owner: priya
groups: [legal]
rank: head
interests: [exposure, litigation]   # additive to the group's
```

Keywords answer **which group**. They cannot answer **who within it**, and the
head/intern pair is the proof: both match `contract` correctly and only one
should speak. That distinction is about the stakes of the question rather than
its vocabulary, which is exactly the shape a model is good at and a keyword is
not. So rung 5 runs on a shortlist of two or three, never the whole roster.

`rank` stays a declared string, not an ordered hierarchy. Ordering it invites
automatic escalation — "the intern answered badly, so the head speaks now" — and
that is a different feature with its own failure modes.

**The router is a connector.** Same spec, same executor, same templating; `base`
points at whatever model endpoint you like. Oryxa gains a router *slot*, not an
LLM dependency, so it still runs with no API key: unconfigured, rung 5 degrades
to waking the whole shortlist, which is strictly better than today because rungs
1–4 already narrowed it. Its decisions are emitted as `routing.decided` events
carrying candidates, scores and reason — a model choosing who speaks is otherwise
the most opaque thing in the system, and as an event it becomes the most
inspectable.

The router call costs no wall-clock. Agents are a turn behind by design, so it
runs inside a window we already accepted.

### 7.4 Owner precedence

A wake targets Arsh's legal agent. Arsh is in the room, and starts typing.

The agent must not race him. Two answers to one question, one of them from
software wearing his name, is the failure that makes a room feel hostile rather
than staffed.

```yaml
name: legal-associate
owner: arsh
groups: [legal]

owner_present:
  defer_when: typing      # typing | present | never
  then: assist            # yield | assist | wait
  hold: 30s
```

- **yield** — stay silent; the room is the owner's
- **assist** — draft privately to the owner, who sends, edits or ignores it
- **wait** — hold, then answer the room only if the owner does not

Presence should also hold the *room*, not just the owner's own agent: if the
person being indicated at is typing, other agents pause a beat too. That is
ordinary turn-taking, and a room without it talks over people.

Two notes on what this costs:

**`assist` needs a primitive we do not have.** Output today is always a room
event. A draft is output addressed to one person, which cannot ship before read
scoping — a private draft everyone can read is not a draft, it is a lie with
extra steps. `yield` and `wait` need only presence.

**The race is already solvable.** Arsh types at t=0, his agent was woken at t=0
and takes 8s, Arsh sends at t=6. `Cancel` exists and is exercised end to end;
an owner sending cancels their own agent's deferred turn.

### 7.5 What the log is worth

Routing on message text is commodity — anyone can call a model. What no one else
has is the log: who was asked, who answered, whether it was escalated, whether a
human corrected it, whether the critic's objection held.

Separating listening from speaking is what makes that data exist at all. If every
agent answers everything there is no signal in participation; once silence is a
choice, the log records who chose to listen and was right to, and who stayed
quiet on something they should have taken. Feeding that back into rung 5 is the
second iteration, not the first — the history has to accumulate before it is
worth reading.

### Order

Participants first, since read scoping, owner-waking and directed output all wait
on it. Then cursors, which is the change that makes the rest affordable. Then the
free rungs of the ladder, measured before anything semantic is added. Presence
and owner precedence after that, with `assist` last because it needs read scoping
to be honest.

**Not next: NATS.** It would fan events across processes, but sessions are
stateful — one goroutine per session *is* the serialization point, so two
instances subscribed to the same subject would both try to run the same turn.
NATS is a transport; it does not decide who owns a session. The order is session
ownership first, then NATS for scale-out and external consumers. The fan-out is
already isolated behind one type, so it drops in without rework when there is a
problem for it to solve.

---

## 8. Building on Oryxa

Three tiers, and it is worth being honest that only two of them are open.

**Tier 1 — add an agent.** One connector, no Go. Five frameworks and a protocol
cost three backward-compatible additions to core, so this tier is proven rather
than hoped for.

**Tier 2 — build a product on the API.** Twenty endpoints and an SSE stream with
`?since=` replay. The viewer is the existence proof: it is an API client that
happens to ship in the binary, and it can be replaced entirely.

**Tier 3 — extend the runtime.** Closed. Every package is `internal/`, which in
Go is enforced rather than advisory, so nothing here can be imported. That is
currently the right call — `events.Store` gained a method this week, which would
have been a breaking change to a published interface — but it should be a stated
decision rather than something people discover. `events.Store` is the first
thing worth exporting, when someone names a second implementation.

### The UI-builder path

A platform whose users build agents in a UI, not a text editor. It matters
because it is a different consumer from everything else here, and most of what
it needs already exists.

**YAML is not the interface.** A connector is a `Spec`; YAML is how a human
writes one on disk, JSON is what `POST /v1/agents` takes, and a form is a third
rendering of the same structure. A UI builder never sees YAML, and registration
needs no restart and no filesystem access.

`POST /v1/agents/{name}/check` runs a real turn and returns structured
diagnostics already written for humans — that is a Test Connection button, and
its warnings are the copy such a button should show.

Two things are still missing before that path is real:

| | |
|---|---|
| **Spec schema** | a UI has to hand-code its form and cannot validate before submitting, so users learn a connector is malformed from a 400 rather than from the field. `Validate()` already has the messages; they are just unreachable until you post. The same artifact the API contract needs. |
| **Secrets** | `GET /v1/agents` returns a spec verbatim, headers included. A connector written by hand keeps its credential out of the file with `{{env.X}}`; a UI-registered one cannot set server-side environment, so the key goes in literally and is then readable by anyone who can list agents. Needed: `{{secret.name}}`, resolved at call time, write-only over the API, never returned. Composes with read scoping rather than fighting it. |

And one that is now fixed: **registrations are durable.** They were held in memory
only, so a restart lost every API-registered agent while sessions replayed
intact — leaving rooms that failed every turn with *agent no longer registered*,
beside a transcript that had come back fine. A registration is now an event on a
reserved system stream, folded at startup after `LoadDir`, which also buys
attribution and an edit history for each connector. The API wins a name collision
with a file, and the startup banner says when it did.

### Stability

`/v1` is stable. The connector spec is stable and additive — new fields, never
changed meanings. Go packages are internal until 1.0.

---

## 9. Docs

| | |
|---|---|
| [README.md](README.md) | what it is, quickstart |
| [docs/integrating.md](docs/integrating.md) | connecting an agent — recipes, reference, troubleshooting |
| [CONTRIBUTING.md](CONTRIBUTING.md) | how to help; connectors are the best contribution |
| [research/01-landscape.md](research/01-landscape.md) | why this layer is open — the evidence base |
| [design/](design/) | current design: domains, sessions & context, connectors, API |
| [design/archive/](design/archive/) | superseded, kept with the reasoning that killed each one |
