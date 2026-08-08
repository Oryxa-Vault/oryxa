---
name: oryxa-connector
description: Write, debug and verify an Oryxa connector so an agent framework can join a shared multi-user session. Use when connecting a new agent to Oryxa, when `oryxa check` reports warnings, when an agent's answer comes back empty, doubled, or full of the model's reasoning, or when someone asks to put an agent in an Oryxa room.
---

# Writing an Oryxa connector

A connector is one YAML file describing how to call an agent over HTTP. Oryxa's
core knows no framework names; everything framework-specific lives in this file.

**Work by probing, not by reading docs.** `oryxa check` runs a real turn against
the real agent and tells you what came back. Two or three iterations of
check → adjust selector → check beats an hour of guessing at an API.

## The loop

1. Find where the agent listens and what it accepts.
2. Write the smallest connector that could work.
3. `oryxa check <name>` — read the `sample` and any warnings.
4. Adjust `text:` / `when:` / `format:` and repeat until green with no warnings.

Never hand back a connector you have not run `check` against. A file that looks
right but was never executed is worse than no file.

## Minimum viable connector

```yaml
name: my-agent
base: http://{{env.ORYXA_AGENT_HOST}}:5000   # defaults to localhost

turn:
  method: POST
  path: /run
  body:
    prompt: "{{input}}"
  response:
    format: json          # json | sse | ndjson
    text:
      - $.output          # tried in order, first match wins
```

Use `{{env.ORYXA_AGENT_HOST}}` rather than a literal host so the same file works
on a machine and in a container.

## If the agent has its own conversation

Add an `open` step. It runs **once per Oryxa session**, and what it captures is
available afterwards as `{{handle}}`.

```yaml
open:
  method: POST
  path: /threads
  capture:
    handle: $.thread_id

turn:
  path: /threads/{{handle}}/runs
```

No `open`? `{{handle}}` falls back to the Oryxa session id, which you can pass
as your own conversation key.

**Several connectors on one runtime need `{{agent}}`.** Every lane in a room
shares the session id, so `{{handle}}` alone gives four differently-briefed
connectors one remote conversation — the agent then sees four conflicting role
instructions in a single thread and answers as all of them. Use
`{{conversation}}-{{agent}}` as the thread key instead.

## Variables

`{{input}}` · `{{turn}}` (unique per turn) · `{{conversation}}` (session id) ·
`{{handle}}` · `{{agent}}` (this connector's name) · `{{vars.x}}` or `{{x}}` ·
`{{env.X}}` · `{{context}}` ·
`{{context.pinned}}` · `{{context.<key>}}`

Unknown names are left in place, so a typo appears in the request rather than
silently becoming empty. Context keys are the exception — an unwritten key
renders empty, because a room that just started is not a typo.

`{{input}}` is one message rendered as itself, or — when several were said while
your agent was busy — an attributed exchange (`alice: …` / `bob: …`) coalesced
into one turn. A connector needs no change for this; a lone message is unchanged.

## Shared context

Two lines connect an agent to the room's shared state. Neither requires tool
calling, a prompt change, or agent-side awareness of Oryxa.

```yaml
turn:
  body:
    message: "{{context}}\n\n{{input}}"   # read

context:                                  # write
  - key: findings
    from: $text                           # whatever the agent said this turn
```

Rule fields: `key`, `from` (`$text` or a selector), `kind` (`append` default,
or `value`), `when` (gate chunks, same syntax as `response.when`), `pin`.

```yaml
context:
  - key: plan
    kind: value
    from: $.output.plan
    when: $.done        # without this a streaming agent writes every prefix
    pin: true
```

Rules to keep in mind when writing one:

- **`append` unless there is one current answer.** It cannot conflict; that is
  why it is the default.
- **Always gate a selector on a streaming agent.** `from: $.answer` with no
  `when` fills the room with every prefix of the answer.
- **One rule per key.** Two rules on the same key is a validation error, not a
  merge.
- **`$text` is the safe default to start, not to stay on.** Every agent produces
  text, so it always works — but it appends a whole answer every turn. Once you
  know which part other agents actually need, move that part to a `value` rule
  with a selector and the room stops growing.
- **`kind: value` with `from: $text` drifts.** Allowed, and correct for an agent
  with one job. But `$text` captures the whole answer, so when the question
  changes the answer's nature the entry follows: a `plan` key overwritten by a
  status line, then by a number. A selector cannot drift.

A failed or cancelled turn writes nothing. Context is snapshotted at turn start,
so parallel agents do not rewrite each other's questions.

### Keeping a room small

`{{context}}` renders everything and grows with the conversation;
`{{context.pinned}}` renders only entries someone marked as still mattering and
does not. Prefer pinned in prompts, and keep `{{context}}` for short rooms and
for debugging.

An append entry renders its newest 20 items and tells the model when it left
older ones out (`- (6 earlier items not shown)`). Nothing is deleted — the API
and the log keep everything; only the prompt is trimmed.

Each `turn.started` records what its agent was shown: `keys` (what the room
held), `reads` (which bindings this template asked for), `chars` (what those cost
this request), and `elided` (items left out of what it read). A connector that
never mentions context is charged nothing, so `chars` is a real per-agent growth
curve rather than the room's size repeated.

Check these first when an agent starts returning less than it should: a full
model window fails quietly, not with an error.

`oryxa which <agent>` prints the rules a connector declares — the first thing to
check when nothing is appearing in shared context.

## Selectors

Dot notation, `[*]` to expand an array, `[n]` to index, `[key]` / `[!key]` to
filter elements by a field's truthiness.

```
$.output                          $.choices[0].delta.content
$.content.parts[*].text           $.content.parts[!thought].text
```

## Diagnosing what check tells you

| Warning | Cause | Fix |
|---|---|---|
| `cannot reach …` | wrong host/port, agent down | check `base` |
| `no text selector matched` | answer is elsewhere in the payload | read the `sample`; use the raw view in the viewer to see the actual chunks |
| `output looks emitted twice` | agent streams deltas **and** a final aggregated message | `when: $.partial` — count only the deltas |
| `payload carries thought` | reasoning model's scratchpad is in the answer | `$.content.parts[!thought].text`, or `when: $.type == TEXT_MESSAGE_CONTENT` for typed-event protocols |
| `agent returned no parts` | agent produced nothing | check its own logs; reasoning models often need a bigger token budget |
| `no response.format set` | defaulting to json | set `sse` or `ndjson` if it streams |

The last two warnings matter most: both describe connectors that **pass while
being wrong**. An answer containing the model's private reasoning still looks
like an answer, and nothing errors.

## Who answers

Every agent reads every message; only some answer. Declare what yours engages on:

```yaml
interests: [spend, budget, cost, roi]
```

First match wins: explicit `to:` → `@mention` → the agent's name → **a person's
name (nobody answers)** → interest → **chatter like "ok"/"thanks" (nobody)** →
otherwise everyone.

A person named outranks an interest on purpose: "arsh what happened with the
vendor" is for Arsh, and an agent that declared "vendor" answering it is the
behaviour that makes a busy room unusable.

An agent that stays quiet loses nothing. Its cursor holds, so when it is finally
asked its turn covers everything it sat through.

```bash
oryxa wake "a message someone would type" -people priya,arsh
```

Prints who answers and why, and for everyone silent, what would have reached
them. Use it whenever an agent is quieter or louder than expected — it needs no
running server.

## Declare only what is true

```yaml
capabilities: [stream, multi_turn]   # omit cancel unless the agent supports it
```

Oryxa adapts rather than assuming. Declaring `stream` on an agent that returns
one blob gives users a fake streaming UI; declaring `cancel` gives them a stop
button that does nothing.

## Auth and timeouts

```yaml
headers:
  Authorization: "Bearer {{env.MY_AGENT_TOKEN}}"
timeout: 5m        # raise for multi-agent crews that run several model calls
```

Secrets go in the server's environment, never in the connector file.

## What not to do

- Don't parse the framework's internal events into `text:`. Tool calls, state
  deltas and lifecycle events are recorded whole as opaque `activity` on
  purpose — reaching into them couples the connector to the framework's release
  cycle.
- Don't invent fields. The spec is `base`, `headers`, `vars`, `capabilities`,
  `timeout`, `open`, `turn`. If you want a fifth thing, it is probably something
  Oryxa deliberately does not do.
- Don't put the answer selector on a chunk you haven't seen. Run `check`.

## Reference

- Full guide: `docs/integrating.md`
- Verified examples: `connectors/*.yaml` — LangGraph, ADK, Pydantic AI, CrewAI,
  and AG-UI as a protocol
- Starting points: `connectors/templates/`
