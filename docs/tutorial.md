# Tutorial: put your agent in a room

By the end of this you'll have your own agent running in a shared session that
several people can watch and talk to, with a durable history behind it.

Nothing about your agent changes. Oryxa reaches out to it where it already runs.

---

## 1. Start Oryxa

```bash
docker compose up -d
```

That's Oryxa plus Postgres. Open <http://localhost:8080>.

The startup banner tells you what mode you're in — read it, because two of these
should not stay this way outside your laptop:

```
  ├─ store       postgres — postgres://oryxa:****@postgres:5432/oryxa
  ├─ auth        none — anyone who can reach :8080 has full access
  ├─ identity    self-declared — the log records claims, not people
  └─ connectors  4 loaded from /connectors
```

No Docker? `cargo run --bin oryxa -- serve` gets you the same thing with an
in-memory log, and `cargo run --bin oryxa` opens the room view instead of the
browser — it starts its own runtime, with a log that survives the restart.

---

## 2. Try it with the stand-in agent

Before touching your own agent, see the shape of the thing:

```bash
cargo run --bin mockagent   # a fake agent on :9000
```

In the viewer: click **mock-sse** in the sidebar. That runs a real probe turn and
shows what came back. Then **+ new session**, type something, send.

**Now open a second browser tab** at the same URL, change the name in the
composer, and send something else. Two people, one agent, one transcript, both
live. That's the whole product.

---

## 3. Connect your own agent

A connector is one YAML file describing how to call your agent. Start from the
closest thing in [`connectors/templates/`](../connectors/templates), or write the
minimum:

```yaml
# connectors/my-agent.yaml
name: my-agent
base: http://{{env.ORYXA_AGENT_HOST}}:5000

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

`{{env.ORYXA_AGENT_HOST}}` defaults to `localhost` and is `host.docker.internal`
under compose, so the same file works whether Oryxa runs on your machine or in a
container.

Then probe it:

```bash
oryxa check my-agent
```

```
  reachable    ok    http://localhost:5000
  turn         ok    412ms · 1 parts · 88 chars
  sample             Sure — here's what I found…
```

`check` runs a **real** turn. When it's green, the thing that will happen in a
session has already happened once.

### If it isn't green

`check` names the problem. The most common ones:

| It says | Do this |
|---|---|
| `cannot reach …` | wrong host or port in `base`, or the agent isn't running |
| `no text selector matched` | your answer isn't where `text:` looks — read the `sample`, or hit **raw** in the viewer to see exactly what arrived |
| `output looks emitted twice` | your agent streams deltas *and* a final message; add `when: $.partial` |
| `payload carries thought` | a reasoning model is leaking its scratchpad; use `$.content.parts[!thought].text` |

The [integration guide](integrating.md) has recipes for streaming, conversations,
reasoning models and typed-event protocols, plus the full selector reference.

Restart Oryxa to pick up the file (`docker compose restart oryxa`), and your
agent appears in the sidebar.

---

## 4. Give it a memory

If your agent has its own conversation concept — a thread, a session, a
conversation id — tell Oryxa how to start one:

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

`open` runs **once per Oryxa session**. Oryxa rides your agent's memory rather
than replacing it — your agent just has several people talking into it instead
of one.

---

## 5. Put several agents in one room

Click more than one agent before creating the session, or:

```bash
curl -X POST localhost:8080/v1/sessions \
  -d '{"agents":["my-agent","another-agent"]}'
```

One question, one answer per agent. Each gets its own lane — its own queue, its
own conversation — and they answer **in parallel**, so the room costs its slowest
agent rather than the sum of all of them. One agent failing doesn't stop the
others.

---

## 6. Use the shared context

A room has shared state alongside its transcript:

```bash
S=localhost:8080/v1/sessions/$SID/context

# append — add-only, cannot conflict. The default for notes and findings.
curl -X POST $S/findings -d '{"append":"postgres handles concurrent writers"}'

# value — single mutable value under optimistic concurrency
curl -X POST $S/decision -d '{"value":"start with sqlite"}'
```

Write a value based on a version someone else has already replaced and you get a
`409` carrying what's current, so you can merge instead of guessing:

```json
{"error":"stale write to \"decision\": current version is 4",
 "current":"start with sqlite","version":4,"by":"carol"}
```

Loud conflicts beat clever merges — a silent overwrite leaves state that's
syntactically fine and semantically wrong.

Agents reach it too, through their connector rather than through a tool they
have to call:

```yaml
turn:
  body:
    message: "{{context}}\n\n{{input}}"   # read the room

context:                                  # write back to it
  - key: findings
    from: $text
```

Add that to the researcher and its answers land in `findings`; every other agent
in the room sees them on their next turn. Neither agent knows the other exists.

---

## 7. Watch, replay, and survive a restart

```bash
curl -N "localhost:8080/v1/sessions/$SID/stream?since=0"
```

That replays the whole session from the beginning, then follows live. Late join,
reconnect and replay are the same code path, because sessions are a fold over an
append-only log rather than state kept beside one.

Which is also why this works:

```bash
docker compose restart oryxa
```

Sessions, history, per-agent conversation handles and shared context all come
back. A turn that was *running* when the process died is marked interrupted
rather than re-run — your agent may well have finished it, and doing the work
twice is worse than saying the outcome is unknown.

---

## 8. Before anyone else uses it

Three flags, all off by default so this tutorial stays short. None should stay
off beyond your laptop:

```bash
ORYXA_TOKEN=$(openssl rand -hex 32)     # guard the API
ORYXA_TRUST_HEADER=X-Forwarded-User     # identity from your proxy
ORYXA_DATABASE_URL=postgres://...       # durable log
```

Put them in `.env` and `docker compose up -d`.

**On `ORYXA_TRUST_HEADER`:** Oryxa has no user accounts on purpose — your
deployment already has identity. Point this at the header your proxy already
sets and the log records people instead of claims. It's only trustworthy when
nothing can reach Oryxa except that proxy, so bind privately.

---

## Where to go next

- [Integration guide](integrating.md) — the full connector reference
- [`skills/`](../skills) — agent skills, if you'd rather have Claude write your
  connector than write it yourself
- [PLAN.md](../PLAN.md) — what's built, what isn't, and the lines this project holds
