# Integrating your agent

Connecting an agent to Oryxa means writing one YAML file. No SDK, no code in
your agent, no changes to how it works.

**The idea:** Oryxa needs to know how to make two HTTP calls — optionally *open*
a conversation, then *run a turn* — and where the answer text sits in the
response. That is the whole contract. Everything else your agent does stays
yours.

---

## In 60 seconds

Your agent accepts a prompt on some HTTP endpoint and returns text. Describe it:

```yaml
# connectors/my-agent.yaml
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
      - $.output
```

Then check it against the real thing:

```bash
oryxa check my-agent
```

```
  reachable    ok    http://localhost:5000
  turn         ok    412ms · 1 parts · 88 chars
  sample             Sure — here's what I found…
```

That's it. `oryxa serve`, open the viewer, and your agent is in a room.

**`check` runs a real turn.** Nothing is simulated. When it is green, the thing
that will happen in a session has already happened once.

---

## What a connector describes

Four things, and only these:

| | |
|---|---|
| **base** | where your agent is |
| **open** *(optional)* | how to start a conversation, run once per session |
| **turn** | how to send one message and read the reply |
| **capabilities** | what your agent supports, so nothing is faked |

If you find yourself wanting a fifth, it is probably something Oryxa
deliberately doesn't do — see [Where the line is](#where-the-line-is).

---

## Pick the recipe that matches your agent

### 1. One request, one response

The common case: a Flask/FastAPI/Express route, a Lambda, anything that returns
a finished answer.

```yaml
turn:
  method: POST
  path: /run
  body:
    prompt: "{{input}}"
  response:
    format: json
    text:
      - $.output      # tried in order; first match wins
      - $.response
      - $.text
```

### 2. Streaming (SSE)

```yaml
turn:
  method: POST
  path: /stream
  body:
    prompt: "{{input}}"
  response:
    format: sse       # or ndjson
    text:
      - $.delta
```

Each SSE `data:` line is parsed on its own and its text appended. If your agent
streams token deltas, they concatenate; the viewer renders them live.

### 3. Your agent has conversations

If your agent keeps its own memory — a thread, a session, a conversation id —
give it an `open` step. It runs **once per Oryxa session**, and whatever it
captures is available afterwards as `{{handle}}`.

```yaml
open:
  method: POST
  path: /threads
  capture:
    handle: $.thread_id

turn:
  method: POST
  path: /threads/{{handle}}/runs
  body:
    input: "{{input}}"
```

**Oryxa rides your session model rather than replacing it.** Your agent's memory
works exactly as it does today — it simply has several people talking into it
instead of one.

No `open` step? `{{handle}}` falls back to the Oryxa session id, so you can pass
that as your own conversation key and get continuity for free.

### 4. Reasoning models

Reasoning models emit their scratchpad alongside the answer. If you don't
exclude it, the model's private thinking ends up in the reply — and it *looks*
like a plausible answer, so nothing errors.

Two shapes, depending on how your framework marks it:

```yaml
# flagged per element (Google ADK: "thought": true)
text:
  - $.content.parts[!thought].text

# its own event type (AG-UI: REASONING_MESSAGE_CONTENT)
when: $.type == TEXT_MESSAGE_CONTENT
text:
  - $.delta
```

### 5. Your agent streams deltas *and* a final message

Many streaming APIs send incremental chunks and then repeat the whole thing as a
final aggregated message. Count both and the answer appears **twice, verbatim**.

```yaml
response:
  format: sse
  when: $.partial     # only the deltas
  text:
    - $.content.parts[!thought].text
```

`check` warns you when it sees this, so you rarely have to spot it yourself.

---

## Reference

### Variables

Anywhere in `path`, `body`, or `headers`:

| | |
|---|---|
| `{{input}}` | the turn's text |
| `{{conversation}}` | the Oryxa session id |
| `{{handle}}` | captured by `open`, else the session id |
| `{{turn}}` | unique per turn — for protocols needing a fresh run id |
| `{{vars.x}}` or `{{x}}` | from the connector's own `vars:` block |
| `{{env.X}}` | the Oryxa server's environment |

An unknown name is **left in place** rather than becoming an empty string, so a
typo shows up in the request instead of silently vanishing.

### Selectors

Deliberately smaller than JSONPath — enough for real payloads without inheriting
an engine's edge cases.

| | |
|---|---|
| `$.a.b` | dot notation |
| `$.items[*]` | expand an array |
| `$.items[0]` | index |
| `$.parts[!thought]` | keep elements where a field is falsy or absent |
| `$.parts[thought]` | keep elements where a field is truthy |

If nothing matches, the chunk is kept as opaque `activity` and `check` warns.
Text you didn't expect beats silence you can't debug.

### `when:` — which chunks count

```yaml
when: $.partial                        # truthy
when: $.type == TEXT_MESSAGE_CONTENT   # equals
when: $.type != REASONING_CONTENT      # not equals
```

Gated-out chunks become `activity` rather than disappearing, so a mis-set `when`
is visible in the log instead of hiding behind an empty reply.

### Capabilities

Declare only what is true. Oryxa adapts rather than assuming.

| | If absent |
|---|---|
| `stream` | the turn is delivered whole when it completes |
| `multi_turn` | each turn starts fresh |
| `cancel` | a running turn plays out; the UI says so instead of offering a dead button |

Nothing is required except `turn`. An agent with only that still works — it is
just one-shot.

### Auth, headers, timeouts

```yaml
headers:                                    # applied to every call
  Authorization: "Bearer {{env.MY_TOKEN}}"
timeout: 5m                                 # default 5m; raise it for slow crews
```

Secrets belong in the server's environment, referenced with `{{env.X}}` — not in
the connector file.

### Who Oryxa says is talking

Input carries an author. By default it is whatever the caller claims. Run the
server with `-trust-header X-Forwarded-User` (or whichever header your proxy
sets) and it comes from there instead, so the log records people rather than
claims. Oryxa has no accounts of its own on purpose — your deployment already
has identity.

---

## When it doesn't work

`oryxa check <agent>` is the first thing to run. It answers *is it me or is it
them* without starting a session.

| What you see | What it means | Fix |
|---|---|---|
| `cannot reach …` | wrong host/port, or the agent isn't up | check `base` |
| `open failed: … 404` | the open path is wrong | compare against your agent's API |
| `open succeeded but captured no handle` | `capture` path doesn't match | print the open response, fix the path |
| `no text selector matched` | output arrived, but not where `text:` looked | see the `sample`, or use **raw** in the viewer |
| `agent returned no parts` | your agent sent nothing | check its own logs — often a token budget |
| `output looks emitted twice` | deltas plus a final message | add `when:` |
| `payload carries thought` | reasoning is leaking into the answer | add `[!thought]` or a `when:` equality |
| answer is there but garbled | wrong `format` | `sse` vs `ndjson` vs `json` |

**The raw view** in the viewer shows every chunk exactly as your agent sent it.
When a selector isn't matching, that is the fastest way to see what actually
arrived rather than guessing.

---

## Worked examples

Five connectors in [`connectors/`](../connectors), all verified against real
servers and a real model. Each is a different shape:

| File | Agent | What it demonstrates |
|---|---|---|
| [`langgraph.yaml`](../connectors/langgraph.yaml) | `langgraph dev` | per-thread URL, `open` capture, SSE token deltas |
| [`adk.yaml`](../connectors/adk.yaml) | `adk api_server` | ids in the body, reasoning filter, `when:` gate |
| [`pydanticai.yaml`](../connectors/pydanticai.yaml) | FastAPI wrapper | a framework with no server of its own |
| [`crewai.yaml`](../connectors/crewai.yaml) | FastAPI wrapper | non-streaming; multi-agent *inside* one turn |
| [`bee-agui.yaml`](../connectors/bee-agui.yaml) | any AG-UI agent | typed events, tool calls, a whole protocol |

Templates to copy from — including a generic HTTP one and an AG-UI protocol one — live in
[`connectors/templates/`](../connectors/templates).

---

## Where the line is

Three things Oryxa will not do, because they are what make "works with any
framework" true rather than aspirational:

**We never look inside your agent.** Text is text; everything else it emits —
tool calls, state deltas, sub-agent chatter, internal events — is recorded whole
as opaque `activity` and never interpreted. Parsing your framework's event schema
would tie us to its release cycle.

**We never touch your prompts or your orchestration.** Multi-agent, planning,
routing, memory: all yours. A CrewAI crew running six model calls inside one turn
is one turn to Oryxa.

**`text:` is a display hint, not a judgement.** It decides what the default
transcript shows. Everything is recorded regardless, and **raw** shows it.

---

## What Oryxa adds

Once your agent is connected, it gains things it did not have:

- **several people in one conversation** — each turn attributed, queued in order
- **a live stream** anyone can join, replayed from the beginning
- **an append-only event log** — late join, replay and audit are one mechanism
- **rooms with several agents**, each keeping its own conversation with its own
  framework, one turn at a time

None of which required your agent to know Oryxa exists.
