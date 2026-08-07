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
| `{{agent}}` | this connector's own name |
| `{{vars.x}}` or `{{x}}` | from the connector's own `vars:` block |
| `{{env.X}}` | the Oryxa server's environment |
| `{{context}}` | everything the room has agreed, as text |
| `{{context.pinned}}` | just the curated set |
| `{{context.<key>}}` | one entry |

An unknown name is **left in place** rather than becoming an empty string, so a
typo shows up in the request instead of silently vanishing.

Context keys are the one exception: `{{context.plan}}` for a key nobody has
written yet renders empty. A missing var is a broken config; a missing context
key is just a room that started five seconds ago, and putting literal braces in
front of a model to report that is worse than putting nothing.

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

### Shared context

A room has shared state: notes, findings and decisions that everyone in it can
read. Two lines connect your agent to it, and your agent does not have to know
any of this is happening.

**Reading** — splice it into whatever your framework calls the prompt:

```yaml
turn:
  body:
    message: "{{context}}\n\n{{input}}"
```

**Writing** — say what your agent leaves behind:

```yaml
context:
  - key: findings
    from: $text          # whatever the agent said this turn
```

That is the whole feature. The agent above now contributes every answer to
`findings`, and every other agent in the room sees it on their next turn.

| field | |
|---|---|
| `key` | the entry to write |
| `from` | `$text`, or a selector into the response payload |
| `kind` | `append` (default) or `value` |
| `when` | gate which chunks a selector reads — same syntax as `response.when` |
| `pin` | mark the entry as part of `{{context.pinned}}` |

`append` is the default because it cannot conflict. Use `value` for state with
one current answer:

```yaml
context:
  - key: plan
    kind: value
    from: $.output.plan
    when: $.done          # a streaming agent emits every prefix of its answer
    pin: true
```

**`kind: value` with `from: $text` drifts.** It is allowed, and it is right when
an agent has exactly one job — whatever it last said is the current answer. But
`$text` captures the *whole* answer, so when the question changes the answer's
nature, the entry follows and its name stops describing its contents. A planner
asked "what should we do?" writes a plan; asked "write the status line" the same
rule overwrites `plan` with a status line, and asked "what was the error rate?"
overwrites it with a number. A selector pins the rule to one field and cannot
drift, which is why `$.output.plan` is the example above and `$text` is not.

Three things worth knowing:

- **Context is snapshotted when a turn starts.** Another agent finishing
  mid-flight does not rewrite the question yours was asked.
- **A failed or cancelled turn writes nothing.** Half an answer recorded as a
  finding is worse than no finding, because the next agent can't tell.
- **Rules are declarative, not a tool your agent calls.** Nothing needs tool
  calling, no prompt changes, and a model cannot decline to record what it found.

Why not an Oryxa tool the agent calls instead? Because it would only work on
frameworks with tool calling, would need a prompt change per agent, and would put
recording the result at the model's discretion. This works everywhere.

### Keeping a room small

`from: $text` with the default `append` grows the room by a whole answer every
turn, and `{{context}}` puts all of it in front of every agent. Left alone, the
prompt grows with the length of the conversation until the model's window ends
it — not with an error, but by quietly returning less than it should.

Three habits, in the order they pay off:

**Write values, not transcripts.** `$text` append is the right way to start,
when you don't yet know what an agent produces that others need. It is the wrong
place to stay. A `value` entry with a selector holds one current answer and never
grows:

```yaml
context:
  - key: hit_rate
    kind: value              # overwrites; the room stays one line
    from: $.metrics.hit_rate
    when: $.done
```

Three turns of `$text` gave us three paragraphs all restating the same number.
The same three turns of the rule above give one number, current.

**Read the curated set.** `{{context.pinned}}` renders only entries someone
marked as still mattering. Prefer it in prompts and keep `{{context}}` for
debugging and for rooms you know are short:

```yaml
turn:
  body:
    message: "{{context.pinned}}\n\n{{input}}"
```

**Know that long lists are trimmed.** An append entry renders its newest
`MaxItemsPerEntry` items — 20 — and says so in the prompt when it drops any:

```
findings:
- (6 earlier items not shown)
- the api is rate limited
```

This is a rendering bound, not a delete. `GET /v1/sessions/{id}/context` and the
event log still hold every item; only the prompt is trimmed.

**And the trimmed part can be summarised rather than just counted.** Start Oryxa
with `-summariser <connector>` and once more than ten items have fallen off, the
marker becomes what they said:

```
findings:
- (30 earlier items, summarised) Checkout returned 503s from 14:02, traced to a
  connection pool capped at 10 against 40 workers. A rollback was attempted twice.
- the error rate is back under 1 percent
```

The summariser is an ordinary connector — Oryxa gains a slot, not a model
dependency, and unconfigured the count-only marker stays. Three properties are
worth knowing because they are what make it safe:

- **Written once, replayed as data.** A summary is a model's output and cannot be
  recomputed, or the room would read differently after every restart. It is an
  event carrying its own text.
- **Built from the original items, never from the previous summary.** Summarising
  a summary loses a little each pass; a long room would end up describing itself
  in generalities.
- **Off the turn path.** It runs after an append, not during a turn, so a room's
  bookkeeping never delays an answer someone asked for. A failure is recorded as
  `rollup.failed` and retried when the tail next grows. Trimming the stored
fold instead would need its own event to survive a restart, and a room whose
history depended on when the server last came up would not be a history.

Each `turn.started` event records what its agent was actually shown:

```json
{"agent": "reporter", "context": {
  "version": 279, "keys": ["findings", "plan"],
  "reads": ["context.pinned"], "chars": 31
}}
```

`keys` is what the room held; `reads` is which bindings this connector's template
asked for, and `chars` is what those cost the request. They are separate numbers
because most connectors read none of the room — quoting the room's size as their
prompt size would make every one of them look about to overrun a window it never
touches. Above, the room holds findings and a plan, but the reporter reads only
the pinned set, so it is charged 31 characters rather than the room's full size.

`chars` is the growth curve — plot it per agent to see the wall before you hit
it, and to see that a pinned-only reader stays flat while the room grows.
`elided` counts only what nothing speaks for — items a rollup covers are
represented rather than missing. It warns that a turn answered from a partial room, and follows what was
read: a reader that never saw a trimmed list is not warned about it.

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

### A turn that finishes without saying anything

This is the one that wastes an afternoon, because it isn't an error — the
request worked, the turn succeeded, and the room simply stays quiet. Oryxa emits
a `turn.empty` event whenever a turn completes with no text, saying which of two
things happened:

```json
{"parts": 0, "text_parts": 0, "reason": "the agent sent nothing at all"}
```

Nothing arrived. The cause is upstream, not here — most often a token budget
spent on reasoning before the model got to an answer, a rate limit, or a refusal.
Check the agent's own logs.

```json
{"parts": 14, "text_parts": 0,
 "reason": "the agent sent 14 parts and no text came out of them; check these
            selectors against the raw view",
 "text": ["$.content"], "when": "$.partial"}
```

The payload arrived and nothing readable came out of it. Usually a `text:`
selector that doesn't fit the payload, or a `when:` gate that excluded every
chunk — but it can also be an agent that genuinely answered with an empty
string. Oryxa doesn't guess between those, because a confident wrong guess sends
you to the wrong file. The selectors are echoed so you can compare them against
the raw view, which settles it in seconds.

Neither case fails the turn or writes to shared context: an empty answer recorded
as a finding is worse than no finding.

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
is one turn to Oryxa. `{{context}}` is not an exception — it substitutes where
your connector puts it and nowhere else. Oryxa never adds to a request your
connector didn't ask for.

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
