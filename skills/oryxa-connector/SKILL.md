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

## Variables

`{{input}}` · `{{turn}}` (unique per turn) · `{{conversation}}` (session id) ·
`{{handle}}` · `{{vars.x}}` or `{{x}}` · `{{env.X}}`

Unknown names are left in place, so a typo appears in the request rather than
silently becoming empty.

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
