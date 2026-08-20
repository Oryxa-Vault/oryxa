# Commands

```bash
curl -fsSL https://oryxa.in/install.sh | sh
```

One binary, for the room view and for scripts. It goes in `~/.local/bin`, is
checked against the published checksums before it is installed, and asks for no
privileges. From a clone: `cargo install --path . --bin oryxa`.

`oryxa-shim` is a second binary and is still Go — `go install ./cmd/oryxa-shim`.
It is deliberately not in the Docker image, because it exists to start processes
on the host.

> Connectors are files. Every command reads `./connectors` unless you say
> otherwise, so an installed binary run somewhere else reports no agents — point
> `--connectors` at your directory, or keep them in `~/.config/oryxa/connectors`,
> which is where the room view looks when there is no `./connectors` beside you.

`oryxa help` prints all of this. `oryxa <command> --help` prints one command's
own flags.

## The room view

```bash
oryxa
```

The first run opens on a welcome screen: the keys, and what a room can do to
the machine it is running on. It is shown once and then not again — a splash
that reappears every launch is one people learn to dismiss without reading,
which would defeat the half of it that matters. `oryxa --welcome` brings it
back.

No command, and you are in the rooms. It follows the live stream, so you watch
several agents answer at once rather than reading it back afterwards.

**With no server running it starts one**, in-process, against an append-only
file under `~/Library/Application Support/Oryxa` on macOS and
`~/.local/share/oryxa` elsewhere. Rooms survive quitting it. The runtime is
bound to loopback and has no token, because it belongs to one person on one
machine; `--server` attaches to a real one instead and then nothing local is
started.

| | |
|---|---|
| `enter` | send. A leading `@agent` directs it at one agent instead of letting the room decide |
| `alt+enter` | newline |
| `esc` | back to the room list, or close a panel |
| `ctrl-c` | cancel the running turn — or quit, when nothing is running |
| `↑ ↓ pgup pgdn` | scroll the transcript. Scrolling back to the bottom follows live again |
| `home end ctrl-a ctrl-e` | start and end of what you are typing |
| `n` | in the room list: open a room, choosing agents with `space` |

Commands are typed into the same box:

```
/context      what the room knows, alongside the transcript
/cancel       stop every running turn
/close        close the room
/key NAME     issue a key that speaks as NAME, shown once
/as NAME      speak as someone else
/raw          show the agent's opaque activity too
/rooms  /new  the room list, or another room
/help  /quit
```

**A coding agent asking permission stops the view and asks.** An ACP agent
blocks its lane until someone decides, so the request is put in front of you
with the agent's own options and answered with one key. Nothing else about the
room is interrupted: other lanes keep running while one waits.

## In an editor

```
oryxa acp                   serve a room to an editor over ACP
```

Not run by hand. An editor launches it, and one ACP session is one Oryxa room —
which makes the agent panel a seat in that room rather than a private chat. The
person in the editor sees every agent answer, attributed, and sees what a
colleague typed in a terminal on the other side of the room. Cancelling in the
panel cancels the room's turns.

In Zed, `~/.config/zed/settings.json`:

```json
{
  "agent_servers": {
    "Oryxa": {
      "command": "/absolute/path/to/oryxa",
      "args": ["acp", "--agents", "claude-code-local,codex-local"],
      "env": {
        "ORYXA_CONNECTORS": "/absolute/path/to/your/connectors",
        "ORYXA_WORKSPACE": "/a/checkout/an/agent/may/write/to"
      }
    }
  }
}
```

Then pick **Oryxa** in the agent panel. With no server running it starts one on
the usual port, so `oryxa` in a terminal joins the same room; with one already
running it attaches, and the room outlives the editor.

Permission requests are the one thing the panel does not answer. The room is the
permission authority, so the panel says who is waiting and the decision is made
in the room view or with `oryxa approve`.

| | |
|---|---|
| `--agents a,b` | the agents a new room is opened with — required, since only the editor's configuration knows which |
| `--room ID` | join this room instead of opening one |
| `--as NAME` | who the person in the editor speaks as |

## Server

```
oryxa serve                 run the API and viewer
oryxa-shim -agents FILE     serve command-line agents over HTTP
```

The viewer is embedded in the binary and served at the same address, for the
people in the room who would rather have a browser tab.

## What an agent in a room can see

An HTTP connector decides that in its request body. An ACP connector has no
body, so it says it in `acp.prompt`:

```yaml
acp:
  command: ...
  prompt: "{{context}}\n\n{{input}}"   # default: "{{input}}"

context:                                # the other half
  - key: findings
    from: $text
    last: true
```

Without both, an ACP agent is in the room without being able to see it. Its own
history it keeps — one ACP session lives for the life of the lane — but the
other agent's answers are invisible, and a room of coding agents is really
several private chats that happen to share a transcript.

`{{context}}` is bounded where it is rendered: twenty items an entry, oldest
first to fall off, `--summariser` deciding whether what fell off is said or
counted. So context grows to a limit and stops rather than to the size of the
room. Each item is attributed, because "codex thinks X" is answerable and an
anonymous fact is not.

## Connectors

Read files, so they work before anything is running.

```
oryxa agents                list configured connectors
oryxa which <agent>         where a connector points, and which file said so
oryxa check <agent>         probe an agent with a real turn
oryxa wake "a message"      who would answer it, and why
```

`oryxa which` exists because `base` is templated — the same connector resolves
differently on a machine and in a container, and *"it works in my shell but not
in the server"* is otherwise a confusing afternoon:

```
  langgraph

  file         connectors/langgraph.yaml
  base         http://{{env.ORYXA_AGENT_HOST}}:2024
  resolves to  http://localhost:2024
  turn         POST /threads/{{handle}}/runs/stream
  open         POST /threads

  ORYXA_AGENT_HOST=localhost — a containerised server resolves this differently
```

## Rooms

Talk to a running server. Everything the room view does, one command at a time.

```
oryxa open <agent>...       start a room
oryxa send <session> TEXT   say something   (--as name, --to agent, -f to follow)
oryxa tail <session>        follow the live stream
oryxa replay <session>      print the history
oryxa sessions              list rooms
oryxa cancel <session>      stop what is running   (--agent NAME for just one)
oryxa context <session>     read or write shared context
oryxa key <session> NAME    issue a key that speaks as NAME
oryxa approve <session>     answer an agent waiting for permission
```

A whole room from the shell:

```bash
SID=$(oryxa open langgraph crewai --json | jq -r .id)
oryxa send $SID "should we use event sourcing?" -f
oryxa context $SID --append decisions --value "revisit after the spike"
oryxa replay $SID
```

Stopping is per-agent, because the lanes are independent — stopping the room to
stop one agent throws away what every other agent had in flight. Short names
work the way they do everywhere else, so `codex` stops `codex-local`:

```bash
oryxa cancel $SID --agent codex   # the others keep working
oryxa cancel $SID                 # everything
```

`approve` with no other flag lists what is waiting and how to answer it:

```bash
oryxa approve $SID                                   # what is waiting
oryxa approve $SID --option allow-once               # answer it
oryxa approve $SID --interaction i_3f2a --deny       # when several are waiting
```

## Flags

Every command takes `--json` for scripting.

| | |
|---|---|
| `--connectors DIR` | connector directory (`ORYXA_CONNECTORS`) |
| `--server URL` | a running Oryxa (`ORYXA_URL`) |
| `--token TOKEN` | API token (`ORYXA_TOKEN`) |
| `--secret SECRET` | room secret, for a room this machine did not open (`ORYXA_SESSION_SECRET`) |

Rooms opened on this machine are remembered in `~/.config/oryxa/rooms.json`, at
mode 0600, so `--secret` is for joining someone else's.

### `serve`

| | |
|---|---|
| `--addr :8080` | listen address |
| `--db DSN` | postgres; in-memory if unset (`ORYXA_DATABASE_URL`) |
| `--event-file PATH` | an append-only file, for a runtime that belongs to one person (`ORYXA_EVENT_FILE`) |
| `--token TOKEN` | require this token (`ORYXA_TOKEN`) |
| `--trust-header HEADER` | identity from your proxy (`ORYXA_TRUST_HEADER`) |
| `--summariser AGENT` | roll long context lists up (`ORYXA_SUMMARISER`) |
| `--admin-token TOKEN` | required to add or remove an agent (`ORYXA_ADMIN_TOKEN`) |
| `--room-turns-per-min N` | turns one room may start (default 30, `0` = off) |
| `--turns-per-min N` | turns across the server (default 120, `0` = off) |
| `--allow-private-agents` | API-registered agents may reach private addresses |
| `--reset` | erase the log on start (`ORYXA_RESET`) — development only |
| `-e, --express` | grant every permission without asking (`ORYXA_EXPRESS`) |

What each of these is for: [running.md](running.md).

### `oryxa-shim`

| | |
|---|---|
| `-addr 127.0.0.1:8090` | listen address |
| `-agents shim.yaml` | agent definitions (`ORYXA_SHIM_AGENTS`) |
| `-token TOKEN` | require this bearer token (`ORYXA_SHIM_TOKEN`) |

Open to anyone who can reach the port if `--token` is empty, and reaching that
port is enough to run what is in the config file.

## Express

```bash
oryxa -e                 # the room view, with its own runtime
oryxa serve --express
oryxa acp --express      # the editor seat
```

`--express` answers every permission request with the agent's own allow option
instead of waiting for a person. It is a real grant: a coding agent can then
write anywhere in its workspace and run what its connector permits, and nobody
is asked first. Point it at a checkout you can throw away, and read
[SECURITY.md](../SECURITY.md) before using it anywhere you cannot.

Three things it deliberately does not do:

- It never picks **always allow** when the agent also offers **allow once**. Always
  is the agent deciding it need not ask again, which takes the next action out
  of the log; once keeps every action a recorded request with a recorded answer.
- It never invents an answer. An agent offering no allow option at all is left
  waiting for a person, and the refusal is recorded.
- It never applies to a server it merely attached to. `--express` configures a
  runtime you start; a room on someone else's server answers by that server's
  rules, and the flag says so rather than silently doing nothing.

Every decision goes through the same path a person's does and is recorded the
same way, attributed to `express`:

```
interaction.requested            actor=claude-code-local
interaction.resolution_requested actor=express    allow
interaction.resolved             actor=express    allow
```

The room view shows **⚡ express** in the header for as long as it is on, and
`serve` says so in its startup banner. A mode this consequential should never be
something you have to remember you turned on.

## Installing

| | |
|---|---|
| `ORYXA_VERSION` | install a particular tag instead of the latest release |
| `ORYXA_BIN_DIR` | install somewhere other than `~/.local/bin` |

The installer never uses sudo. If it cannot write where it is pointed it says
so and stops.
