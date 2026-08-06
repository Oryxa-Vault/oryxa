# Oryxa — Engagement Modes

**Date:** 2026-08-05
**Status:** draft for review
**Builds on:** [design/04-stack.md](04-stack.md)

---

## 1. Three modes

| Mode | Who decides to share | Agent change | For |
|---|---|---|---|
| **Tool** | the agent, explicitly | one config line (their side) | deliberate collaboration; agent knows it's handing off |
| **Trigger** | a user-written condition | none | "go multiplayer when X" — the default for most people |
| **Always-on** | nobody; it's on | none | teams already running agents together |

*(Reading the third as the passive capture from 04 — say if you meant something else.)*

---

## 2. They are one mechanism, not three

The gateway captures every turn regardless of mode. What differs is only **when a session
becomes shared**:

> **Capture is always on. The modes are activation policies over the same capture.**

- Tool → activate when the agent says so
- Trigger → activate when the user's condition is met
- Always-on → activate at session start

One pipeline, three policies. Not three integrations to build and maintain — which matters,
because "three ways in" is normally how a project ends up with three half-working ways in.

---

## 3. Capture private, promote on activation

Capturing everything while sharing only sometimes needs a privacy answer, and we already have
the concept: **scopes**.

```
agent-local   ← everything lands here, always
      │
      │  activation
      ▼
session-shared ← visible to other participants
```

Every output is logged agent-local from the first turn. That's enough for the agent's own
ledger, provenance, and replay. **Activation promotes the lane**, it doesn't start recording.

### 3.1 Backfill is free

Because capture predates activation, a session that activates on turn 12 doesn't start sharing
from turn 12 — the full history is already in the log and gets promoted with it. Agents joining
later get the whole story.

The "one turn late" problem that would otherwise sink a triggered mode simply doesn't exist,
and it's a direct dividend of always-on capture. **Mode 3 is what makes mode 2 cheap.**

---

## 4. How the trigger layer works

The user writes the activation condition in natural language:

```yaml
activate_when: |
  The agent is working on something that affects shared state,
  or needs a result another agent is producing.
```

Four properties make this affordable and safe:

**Out-of-band.** Trigger evaluation is a *separate* call to a cheap model. It is never inserted
into the agent's request — byte-identical forwarding (04 §1) is not negotiable, and the trigger
layer does not get to be the exception.

**Post-turn, off the critical path.** We forward the request, stream the response back
untouched, and *then* evaluate. Zero added latency to the agent. Backfill (§3.1) is what makes
evaluating after the fact costless.

**Latched, not per-turn.** Once a session activates it stays activated. So the classifier runs
until the first trigger, not forever — the cost is bounded per session rather than per turn.

**Two-stage funnel.** Cheap structural pre-filters first (did the agent spawn a sub-agent? call
a shared resource? emit a matching shape?), classifier only on what survives. Most turns never
reach the model.

---

## 5. Activation is a one-way door

The safety property that shapes the defaults: **once shared, it cannot be unshared** — other
participants may already have read it. There is no undo.

Three consequences, all of which should be built in from the start rather than added after the
first incident:

1. **Fail closed.** An uncertain classifier does not activate. False negatives are a missed
   collaboration; false positives are a disclosure.
2. **Audit the decision.** Activation is an event — `session.activated` — carrying the trigger
   that fired and the evaluator's reasoning. This lands in the ledger for free and makes "why
   did this become shared?" answerable, which is exactly the question someone will ask.
3. **Confirm mode.** An option where activation requires a human to approve before promotion.
   For anyone whose agents touch data they care about, this is the difference between adoptable
   and not.

An LLM-evaluated condition is nondeterministic, and the failure direction is asymmetric. The
design should lean hard toward under-triggering.

---

## 6. Mode 1: the tool

Oryxa ships an MCP server exposing collaboration operations — `share_context`, `read_shared`,
`activate_session`. If someone wants explicit control, they wire it into their agent themselves.

That's consistent with the boundary from 04: **we don't inject tools, because tools are input,
and input is theirs.** Mode 1 is a thing we offer, not a thing we do to them.

It's also the mode with the best reliability story, since the agent activating deliberately is
unambiguous — no classifier, no inference, no one-way-door risk from a wrong guess.

---

## 7. Open questions

1. **Trigger evaluator model** — bundled small local model, or the user's own provider? *Lean:
   user's own, via a configured cheap model. Shipping a model breaks the single-binary story.*
2. **Does activation promote all agent-local history, or a window?** *Lean: all, with a
   configurable cap — partial history is how you get confidently wrong provenance.*
3. **Can a session deactivate?** Not to unshare (impossible), but to stop *new* sharing. *Lean:
   yes, and it's cheap — worth having.*
4. **Multiple triggers per session?** Different conditions promoting different regions is
   powerful, and probably v2.
5. **Session binding** — still the one blocking gateway code: how does a request map to a
   session? Header, API-key mapping, or URL path.
