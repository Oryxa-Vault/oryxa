# Oryxa

**Multiplayer AI — many people and many agents in one room, live.**

Your Claude Code session and your colleague's Codex session are two private
terminals that will never know about each other. Oryxa puts them in the same
room, on the same question, in front of the same people. Neither CLI was
modified, and neither knows the other is there.

![Claude Code and Codex answering one question at once, watched by two people](docs/media/room.gif)

*Recorded from a real session. The script that produces it is in
[recording/](recording), so it is re-run after a change rather than performed
again by hand.*

[![Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)
[![PRs welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

---

## Try it

```bash
git clone https://github.com/Oryxa-Vault/oryxa && cd oryxa
docker compose up -d && go run ./cmd/mockagent &
open http://localhost:8080
```

A running room with a stand-in agent. Pick the agent in the sidebar, hit **+ new
session**, and talk to it. No Docker:

```bash
go build -o oryxa ./cmd/oryxa && go run ./cmd/mockagent &
./oryxa launch window
```

Both start in the clone because connectors are **files** — `oryxa` reads
`./connectors`, and that is where the working examples live. Once you have your
own, the binary alone is enough:

```bash
go install github.com/Oryxa-Vault/oryxa/cmd/oryxa@latest
oryxa serve -connectors /path/to/yours
```

**As two people:** open the viewer in a second tab and change the name in the
composer. Both tabs are in the same room, watching the same agent.

## Claude Code and Codex in one room

Both CLIs already installed and signed in. The shim gives them an HTTP surface,
and then they are ordinary connectors:

```bash
export ORYXA_SHIM_TOKEN=$(openssl rand -hex 32)
oryxa-shim -agents shim.yaml &     # same shell — both processes need the token
oryxa serve &
oryxa open claude-code codex
```

> **Both agents can write inside their working directory**, and they are
> confined differently — Codex by the OS sandbox, Claude Code by path patterns
> only. Anyone who can reach that room can cause writes to that directory. Point
> it at a checkout you can throw away. [SECURITY.md](SECURITY.md) has the
> detail.

## Connect your own agent

A connector is a **description of HTTP calls**, not code. One file, no Go.

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

Drop it in `./connectors/`, then:

```bash
oryxa check my-agent      # runs a real probe turn and says what came back
```

`check` reports reachability, timing, whether your selectors matched, and
warnings for the ways a connector can pass while being quietly wrong. It answers
*is it me or is it them* without starting a session.

**Verified against real servers and a real model:** LangGraph, Google ADK,
Pydantic AI, CrewAI, AG-UI (any agent), Claude Code, Codex. Seven genuinely
different surfaces, one core, one YAML file each — see
[`connectors/`](connectors).

---

## What it does

**Several agents, one room.** Each gets its own lane: its own cursor into what
the room has said, its own goroutine, its own conversation. They answer **in
parallel**, so a room costs its slowest agent rather than the sum of them. Five
live frameworks answering one question: 15.6s of work in 4.9s of wall clock.

**Not everyone answers everything.** A connector says what it engages on, and
the room works out who a message is for — an agent named, one that declared an
interest, nobody at all when you are asking a colleague or just saying thanks.
Measured on seven live agents: **35 model calls instead of 133**. That is a nice
saving when a call is cheap and a different product when it is a three-minute
coding turn.

**Nothing queues behind a turn.** Say something while an agent is mid-turn and
it picks it up next turn, along with anything else said meanwhile. Eight
messages typed at human speed ran as two turns, not eight.

**The log is the source of truth.** Sessions are a fold over it, which is why
late join, replay and audit are one mechanism rather than three features.
Anyone joining later replays the whole history and then follows live.

**Shared context.** A room has state alongside its transcript that agents read
and write — two lines in a connector, no tool calling, no prompt change, no
awareness that Oryxa exists.

**We never look inside the agent.** Text is text; everything else is passed
through as opaque activity. Parsing a framework's event schema would couple us
to its release cycle.

## Status

**v0.5.** Connectors, rooms, shared context, an event log everything is a fold
over, live stream, viewer, command-line agents through a shim, scoped rooms,
turn budgets, and room keys so a name can be proved rather than claimed.

Not built yet: agents have no owners, so the room's idea of who is in it is
"everyone who has spoken"; no presence; turns are bounded per minute but not in
flight at once; and events carry neither a hash chain nor token counts.
[`PLAN.md`](PLAN.md) carries the full list with the reasoning for each.

Rooms are closed and names can be proved, but **neither is on by default** —
that is what keeps the quickstart two minutes. None of it should be off anywhere
else. [`docs/running.md`](docs/running.md) is the four flags that separate a demo
from something you can leave running.

## Docs

| | |
|---|---|
| [Tutorial](docs/tutorial.md) | your agent in a room, end to end |
| [Integration guide](docs/integrating.md) | recipes, the full connector reference, and a symptom-to-fix table |
| [Running it for real](docs/running.md) | durability, auth, identity, budgets, Docker, endpoints |
| [Commands](docs/cli.md) | the CLI |
| [AGENTS.md](AGENTS.md) | hand this to your coding agent — a runbook it follows to set Oryxa up and connect your agent, with a check after every step |
| [Agent skills](skills/) | let Claude write your connector — it is an iterative probe-and-adjust loop, which is what agents are good at |
| [SECURITY.md](SECURITY.md) | the posture, and what it does not cover |
| [PLAN.md](PLAN.md) | what is built, what is not, and why |

## Contributing

Contributions are genuinely welcome, and small ones count. The highest-value
thing is a connector for a framework we haven't tested — a single YAML file, no
Go needed. [CONTRIBUTING.md](CONTRIBUTING.md) has the three-step version.

If you tried Oryxa and got stuck, an issue saying where you got lost is worth
real work to us: it tells us exactly where the docs are wrong.

## Licence

Copyright 2026 Oryxa. Licensed under the [Apache License 2.0](LICENSE).
