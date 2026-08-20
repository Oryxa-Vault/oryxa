# AGENTS.md

**You are an agent, and someone has asked you to set up Oryxa or connect
something to it. This file is the procedure.**

Work through it in order. Every step has a check. Run the check — do not assume
a step worked because the command exited 0, and do not move on while a check is
failing, because the next failure will be harder to read.

Everything here is verifiable from the repo. If this file and the code disagree,
the code is right and the file is a bug — say so.

---

## Which task is this?

| They said something like | Go to |
|---|---|
| "connect my agent", "add my LangGraph app to Oryxa" | [Task B](#task-b-connect-an-http-agent) |
| "get Oryxa running", "try this out" | [Task A](#task-a-get-it-running) |
| "put Claude Code and Codex in a room", "add my coding agent" | [Task C](#task-c-connect-a-coding-agent) |
| "fix this bug", "add this feature" (to Oryxa itself) | [Task D](#task-d-work-on-the-core) |

If it is Task B or C, **do Task A first**. A connector cannot be debugged
against a server that is not running, and most "my connector doesn't work"
turns out to be that.

## Orientation

Oryxa is a room that several people and several agents share. People talk to the
room; Oryxa drives the agents in it and streams everything back live.

```
  alice   bob   carol          many people, one room
    └──────┼──────┘
           ▼
    ┌─────────────┐
    │   Oryxa     │  owns: input queue, event log, presence, streaming out
    └──────┬──────┘
           │ connector (declarative HTTP or local ACP)
           ▼
    ┌─────────────┐
    │ your agent  │  keeps: prompts, tools, memory, orchestration
    └─────────────┘
```

To the framework, Oryxa is one user. Nothing about the agent changes — no prompt
edit and no awareness that Oryxa exists. A **connector** is a YAML file describing
HTTP calls or an operator-controlled local ACP process. The runtime API can
register HTTP connectors but can never name an ACP command.

---

## Task A: Get it running

**Step 1.** Check Rust is installed.

```bash
cargo --version
```

If there is no cargo, stop and tell the user: either install Rust, or install
the released binary with `curl -fsSL https://oryxa.in/install.sh | sh` and skip
to Task B. Everything below builds from source, which is what you want when the
work is on the core.

**Step 2.** Work from a clone.

```bash
git clone https://github.com/Oryxa-Vault/oryxa && cd oryxa
cargo build --bins
```

**Do this even if `oryxa` is already installed.** Connectors are *files*, and
`oryxa` reads `./connectors` — an installed binary run anywhere else reads
`~/.config/oryxa/connectors` instead and sees none of the examples. An empty
agent list means you are in the wrong directory, not that anything is broken.

**Step 3.** Start the stand-in agent, then the server.

```bash
./target/debug/mockagent &   # :9000 — something real to probe
./target/debug/oryxa serve & # :8080
```

**Check.** In another shell:

```bash
curl -s localhost:8080/v1/agents | head -c 200
```

Expect JSON listing connectors. Connection refused means the server is not up —
read its output before continuing.

**Step 4.** Confirm a turn works end to end.

```bash
./target/debug/oryxa check mock-json
```

Expect `reachable ok`, a timing, and a sample answer. The stand-in connectors
are `mock-json` and `mock-sse` — there is no connector called `mock`.

If this fails, the problem is the setup, not anything the user wrote. Fix it
here, because every later failure will be harder to read.

**Done when:** `oryxa check mock-json` passes. The viewer is at
http://localhost:8080.

---

## Task B: Connect an HTTP agent

This is the common request. It is a loop — write, probe, read, adjust — and it
converges in two or three passes. Do not try to write the perfect connector
first; write the smallest one and let `oryxa check` tell you what is wrong.

### Step 1. Find out what shape their agent is

**Do not guess this. Ask the agent itself.** You need three facts: the URL, what
a turn request looks like, and where the answer text sits in the response.

```bash
curl -sv -X POST http://localhost:5000/run \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"say hello"}' 2>&1 | tail -40
```

Read the output and answer these before writing anything:

| Question | What to look for |
|---|---|
| Does it stream? | `Content-Type: text/event-stream`, or chunks arriving over time |
| One response or many lines? | a single JSON object vs NDJSON vs SSE `data:` frames |
| Where is the text? | the JSON path to the answer — `$.output`, `$.messages[-1].content`, … |
| Does it have conversations? | an endpoint that mints a thread/session id you must reuse |
| Does it need auth? | 401/403, or docs mentioning a key |

If the user has docs for their framework, read those too — but trust the actual
response over the docs, because that is what Oryxa will receive.

### Step 2. Write the smallest connector that could work

`connectors/my-agent.yaml`:

```yaml
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
      - $.output          # tried in order, first match wins
```

Variables you can use:

| | |
|---|---|
| `{{input}}` | what the room said this turn |
| `{{handle}}` | the conversation id, if the agent has conversations |
| `{{context}}` | the room's shared state, rendered |
| `{{context.pinned}}` | just the curated set |
| `{{env.NAME}}` | an environment variable |

### Step 3. Probe it

```bash
oryxa check my-agent
```

This runs a **real turn**. It reads files only, so it works with no server
running. It reports reachability, the captured handle, timing, whether your
selectors matched, and warnings for the ways a connector can pass while being
quietly wrong.

### Step 4. Read the failure and adjust

| `check` says | It means | Do this |
|---|---|---|
| connection refused | agent not running, or `base` wrong | `oryxa which my-agent` prints what `base` resolves to |
| 401 / 403 | auth missing | add `headers:`; use `{{env.VAR}}` rather than pasting a key into the file |
| no text matched | the request worked, your selector did not | curl it again, find the real path, fix `text:` |
| empty answer, no error | selector matched an empty string | check `when:` is not excluding the chunks carrying text |
| the answer is one token | it streams deltas and you are reading one | see the streaming recipe below |
| answer duplicated | it streams deltas *and* sends a final message | [Deltas and finals](docs/integrating.md#5-your-agent-streams-deltas-and-a-final-message) |
| every turn acts like the first | conversation handle not captured or not resent | [Conversations](docs/integrating.md#3-your-agent-has-conversations) |

Then go back to Step 3. **Repeat until `check` is clean** — a warning here is a
bug that will show up later as an agent behaving oddly in a room.

### Step 5. Match the recipe to the shape

If the minimal connector is not enough, the shape you identified in Step 1 tells
you which section to read. Do not invent a mechanism — one of these covers it:

| Their agent | Read |
|---|---|
| streams SSE | [Streaming](docs/integrating.md#2-streaming-sse) |
| has conversations / threads | [Conversations](docs/integrating.md#3-your-agent-has-conversations) |
| is a reasoning model | [Reasoning models](docs/integrating.md#4-reasoning-models) |
| streams deltas *and* a final | [Deltas and finals](docs/integrating.md#5-your-agent-streams-deltas-and-a-final-message) |
| is a command, not a server | [Task C](#task-c-connect-a-coding-agent) |

`connectors/` has seven working examples against real frameworks. **Read the one
closest to their agent's shape before writing from scratch** — it is usually a
five-line diff from a working file rather than a new one.
[`connectors/templates/`](connectors/templates) has starting points.

### Step 6. Put it in a room

```bash
oryxa open my-agent
oryxa send <session-id> "hello" -f
```

**Done when:** the agent answers in the room, not just in `check`. Tell the user
the session id, that `oryxa` on its own opens the room to watch and talk in, and
that the viewer is at http://localhost:8080 for anyone who wants a browser tab.

### Optional: let it read and write the room's shared state

Two lines, and the agent joins the room's thinking without knowing Oryxa exists:

```yaml
turn:
  body:
    message: "{{context}}\n\n{{input}}"   # reading

context:                                  # writing
  - key: findings
    from: $text
```

---

## Task C: Connect a coding agent

A coding agent joins over ACP: one long-lived subprocess and one ACP session per
room-agent lane, so turns stay ordered inside an agent while agents run in
parallel. The adapters are npm packages the ACP registry names, which are the
same ones an editor would launch.

Whoever is in the room brokers permission for ACP lanes. Permission requests
become durable room interactions and remain paused until someone selects one of
the exact options supplied by the agent: the room view (`oryxa`) prompts for
them, `oryxa approve <room>` answers them from a script, and any other client
can use the interaction list and resolve endpoints. Cancelling a turn cancels
its unresolved request; nothing is silently approved.

> **Stop and ask the user before you run this.** Both shipped agents can
> **write inside their working directory**, and anyone who can reach the room
> can make them act. Ask which checkout to point at, and recommend one they can
> throw away. Do not point it at the repository they are currently working in
> unless they say to.

**Step 1.** Confirm the agent is signed in. Oryxa starts processes; it cannot
authenticate for them.

```bash
claude auth status       # or: codex login status
```

Claude Code finds its credentials by `$USER`, so a runtime launched with a
stripped environment fails every turn in about a second with `ACP prompt` and
nothing else.

**Step 2.** Name the workspace, then start the server in the same shell.

```bash
export ORYXA_WORKSPACE=/a/checkout/you/can/throw/away
oryxa serve &
```

**Step 3.** Probe, then open. `oryxa which <agent>` prints what the connector
resolved to, which is the fastest way to find out that it resolved to nothing.

```bash
oryxa check codex-local
oryxa open claude-code-local codex-local
```

**Check.** A coding-agent turn is a whole agentic loop, so it is slow — the
connectors allow 30m. Do not conclude it is broken because it has not answered
in ten seconds; `oryxa tail <session>` shows it working, and the first turn in a
room also pays for the agent's own session setup.

### What confines them, and what does not

| | Claude Code | Codex |
|---|---|---|
| confinement | path patterns only — `Edit(./**)`, `Write(./**)` | OS sandbox — workspace-write |
| remove that and | it writes anywhere (verified: it wrote to `/tmp`) | the sandbox still holds |
| permission | asks the room, and waits | asks the room, and waits |
| session | resumed after a restart, both advertise `loadSession` |

`--tools` makes a tool exist; `--allowedTools` is what stops it asking. A tool
that exists and is not allowed is **denied outright** in headless, which reads
as an agent that will not try rather than one that cannot.

**Never** widen `Edit(./**)` to `Edit(**)` or add a broad `Bash(*)` on the
user's behalf. That removes the only boundary Claude Code has. If they ask for
it, tell them what it costs first.

What may run lives in an operator-controlled ACP connector file and can never be
set over HTTP. `POST /v1/agents` accepts HTTP connectors at runtime; allowing
that endpoint to name a command would be remote code execution behind one shared
token.

---

## Task D: Work on the core

```bash
cargo test --all-targets
cargo fmt --all --check    # must print nothing
cargo clippy --all-targets -- -D warnings
```

CI runs those, plus a test that fails if `openapi.yaml` and the registered
routes disagree in either direction, and a check that no framework name appears
in core code.

### Layout

```
src/bin/oryxa.rs    the binary: server, room view and every command
src/bin/mockagent.rs stand-in agent for testing
src/tui/            the room view — state, keys, drawing
src/cli/            the client half: HTTP, SSE, room secrets, printing
src/runtime.rs      binding a server, for `serve` and for the room view
src/connector/      spec, templating, selectors, HTTP and ACP executors, check
src/session/        the room: membership, lanes, turn loop
src/events/         append-only log
src/api/            HTTP surface
src/sharedctx/      shared context
src/acp_server.rs   Oryxa as an ACP agent, so an editor is a seat in the room
install.sh          the one-URL installer, published to oryxa.in
web/index.html      the browser viewer, embedded into the binary
connectors/         connector files, loaded at startup
openapi.yaml        the HTTP contract, tested against the router
```

### Invariants — do not break these

These are what make "works with any framework" true rather than aspirational. If
a change seems to require breaking one, you have the wrong design — say so
rather than working around it.

1. **The core never branches on which framework it is talking to.** No framework
   name in `internal/`. CI enforces this.
2. **Multi-user, not multi-agent.** Their orchestration stays theirs.
3. **Never look inside the agent.** Text is text; everything else is recorded
   whole as opaque activity and never interpreted. Parsing a framework's event
   schema couples us to its release cycle.
4. **Never touch prompts.**
5. **Events are the truth.** Anything derived is derived. If a feature wants to
   write to a projection, the answer is an event.
6. **The log is append-only.** Never coalesce, never dedupe.
7. **Capabilities are declared, never assumed.** No fake streaming.
8. **One turn at a time per agent, parallel across agents.** An agent's own
   conversation is sequential, so overlapping its turns would misrepresent what
   is underneath. Two agents have two conversations and no reason to wait.

[`PLAN.md`](PLAN.md) is the source of truth for what is built and what is not.
Check it before implementing something that may already be deliberately absent.

---

## Rules

**Stop and ask the user before you:**

- point a writing coding agent at a directory (Task C)
- widen a tool allowlist or sandbox flag
- expose a server on anything but loopback
- put a real API key in a file — use `{{env.VAR}}` and let them set it

**Do not guess these. They are here so you do not have to:**

| | |
|---|---|
| Rust | stable |
| Node | 20+, for the ACP adapters `npx` runs |
| server | `:8080` — API and viewer |
| mockagent | `:9000` |
| verified against | claude-agent-acp 0.69.0, codex-acp 1.4.0 |
| coding-agent timeout | 30m |
| turn budgets | 30/min per room, 120/min per server, `0` disables |
| store | in-memory unless `--db` names a postgres DSN |
| auth | off unless `--token` is set; the startup banner says which |
| licence | Apache 2.0 |

Every command takes `--json`, which is what you should use when parsing output.
Room commands take `--server` and `--token`, falling back to `ORYXA_URL` and
`ORYXA_TOKEN`; connector commands take `--connectors`, falling back to
`ORYXA_CONNECTORS`.

**Report honestly.** If `check` still warns, say so rather than declaring
success — a connector that passes while being quietly wrong is the specific
failure this tooling exists to catch.

## Where else to look

| | |
|---|---|
| [README.md](README.md) | the short version, for humans |
| [docs/tutorial.md](docs/tutorial.md) | the same ground at human pace, end to end |
| [docs/integrating.md](docs/integrating.md) | the full connector reference — every recipe, selector and gate |
| [docs/running.md](docs/running.md) | durability, auth, identity, budgets |
| [docs/cli.md](docs/cli.md) | every command and flag |
| [SECURITY.md](SECURITY.md) | the posture, and what it does not cover |
| [PLAN.md](PLAN.md) | what is built, what is not, and why |
| [skills/](skills/) | this knowledge packaged as agent skills |
