# Oryxa — Sessions & Context

**Date:** 2026-08-05
**Status:** draft for review

The v1 core. Many people, one agent, one shared room.

---

## 1. Session

```
Session: open ──► closed

  people      who's in, presence, who's watching
  queue       pending input
  turn        the one running now, if any
  context     shared state (§4)
  events      the log — everything above is a fold over it
```

A session is created by a person or by API, and it's bound to exactly one configured agent.
Anyone with access can join; joining is cheap and reversible.

---

## 2. The turn loop

```
      ┌──────────────────────────────────────────┐
      ▼                                          │
   [idle] ──input──► [running] ──┬──► [done] ────┘
                         │       ├──► [failed]
                         │       └──► [asking] ──answer──► [running]
                         └── cancel ──► [cancelled]
```

**One turn at a time.** Not our design preference — their thread is sequential. An ADK session
and a LangGraph thread each process one run at a time, so a session that ran turns
concurrently would be lying about what's underneath. We respect their model rather than
inventing on top of it.

Everything else follows: input that arrives mid-turn **queues**.

---

## 3. Many people, one queue

**Anyone can send.** Input carries who sent it, always.

**The queue is visible.** People see what's pending, in order, and can withdraw their own entry
before it runs. A queue you can't see is indistinguishable from a hang — this is the difference
between "the room is busy" and "is it broken?"

**Turns run sequentially, one per input.** Alice's question is answered, then Bob's. We don't
merge inputs into one turn: merging is clever and surprising, and surprising is expensive when
three people are watching.

**Attribution reaches the agent.** Input is composed as `[alice] what's the status?` by default,
configurable off. Oryxa owns the input path now, so composing it is legitimate — but it's still
a change to what the agent sees, so it stays a visible, documented rule rather than a hidden
one.

### 3.1 Cancel

If the connector declares `cancel`, any participant can stop a running turn. If it doesn't, the
turn plays out and the UI says so. Never fake a cancel — a stop button that doesn't stop is
worse than no stop button.

---

## 4. Context

A **keyed store** the whole room shares. Two entry types, one flag:

| | Type | Semantics |
|---|---|---|
| `append` | list | add-only; no conflicts possible |
| `value` | single | mutable, optimistic concurrency |

**`append` is the default** and should be the recommended shape. It's what most content
actually is — findings, notes, decisions — and every entry it absorbs is a merge problem nobody
has to solve.

**`value` uses OCC.** Read returns the version; write must supply it; stale write → `409` with
the current value. It doesn't prevent conflicts, it makes them **loud**. That's the point: a
silent overwrite produces state that's syntactically fine and semantically wrong, which surfaces
later as the agent behaving oddly and gets blamed on the model. Last-write-wins is rejected —
it fails under clock skew between participants.

### 4.1 Pinned

Any entry can be **pinned**. Pinned entries are the small, curated set the agent should always
know — goals, constraints, key decisions.

```
context
├── pinned      ──► composed into every turn's input   (bounded, previewable)
└── everything  ──► available on request via read()
```

This is the answer to "how does the agent get the team's context" without dumping an unbounded
log into a prompt:

- **bounded by construction** — a handful of entries, with a byte budget
- **curated by people** — humans decide what matters, not a heuristic
- **previewable** — the UI shows exactly what will be sent, before it's sent

Unpinned context isn't hidden, just not automatic. The agent reads it when it wants it.

---

## 5. Ask — the agent talks to the room

The genuinely multi-user moment. Mid-turn, the agent asks a question; the turn pauses; **anyone
in the room can answer**; first answer wins and is logged with who gave it.

Design points:

- The question goes to **the room, not a person.** Routing to an individual reintroduces the
  single-user assumption we exist to remove.
- **Timeout is configurable.** On expiry the agent receives "no answer" rather than an error —
  it decides what to do with that. Hanging forever is unusable; failing the turn discards real
  work.
- Requires connector `callback`. Without it, `post` still works one-way and `ask` is absent —
  declared, not silently missing.

---

## 6. Events

Everything above is a fold over this log. Append-only, ordered, attributed.

```
session.created  session.closed
user.joined      user.left
input.submitted  input.withdrawn
turn.started     turn.finished     turn.failed     turn.cancelled
output.part                                 # text deltas + opaque activity
context.appended context.set      context.pinned  context.unpinned
ask.raised       ask.answered     ask.timed_out
```

**Never coalesce, never dedupe.** Compaction is fine on derived views; the log is append-only,
always. It's the source of truth for replay, audit, and catch-up, and each of those breaks
quietly if writes get optimized away.

---

## 7. Joining late is the normal case

Someone opening a session mid-run gets, in one response: the event history, the current context
snapshot, the running turn's output so far, and then the live stream.

This is free — it's just reading the log and subscribing. Which is the real argument for the log
being the source of truth rather than a side-effect: **late join, replay, and audit are the same
mechanism**, so none of them needs its own feature.

---

## 8. Surface

```
POST   /sessions                        create
GET    /sessions/:id                    state + people + queue
POST   /sessions/:id/input              send (queues if busy)
DELETE /sessions/:id/input/:id          withdraw
POST   /sessions/:id/cancel             stop running turn
GET    /sessions/:id/context            all entries
POST   /sessions/:id/context/:key       append / set (If-Match for values)
POST   /sessions/:id/context/:key/pin   pin / unpin
POST   /sessions/:id/ask/:id/answer     answer the room's question
GET    /sessions/:id/stream             SSE: everything, from any point
GET    /sessions/:id/events?since=      raw log
```

---

## 9. Open

| # | Question | Lean |
|---|---|---|
| 1 | Pinned budget — bytes, entries, or both? | both, with a visible meter; silently truncating pinned context would be the worst failure here |
| 2 | Can people edit each other's context entries? | yes for `value`, no for `append` — append is a record of who said what |
| 3 | Does the queue survive a restart? | yes; it's in the log |
| 4 | Multiple sessions against one framework thread? | no — one session, one thread, or their memory gets confusing |
| 5 | Who can close a session? | creator + anyone with write access; it's reversible (reopen), so keep it loose |
