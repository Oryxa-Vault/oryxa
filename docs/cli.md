# Commands

```bash
go install ./cmd/oryxa      # or: go build -o oryxa ./cmd/oryxa
```

`oryxa help` prints all of this. `oryxa <command> -h` prints one command's own
flags.

## Server

```
oryxa serve                 run the API and viewer
oryxa launch window         run and open the viewer in a browser
oryxa-shim -agents FILE     serve command-line agents over HTTP
```

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

Talk to a running server.

```
oryxa open <agent>...       start a session
oryxa send <session> TEXT   say something   (-as name, -f to follow)
oryxa tail <session>        follow the live stream
oryxa replay <session>      print the history
oryxa sessions              list sessions
oryxa context <session>     read or write shared context
oryxa key <session> NAME    issue a key that speaks as NAME
```

A whole session from the shell:

```bash
SID=$(oryxa open langgraph crewai -json | jq -r .id)
oryxa send $SID "should we use event sourcing?" -f
oryxa context $SID -append decisions -value "revisit after the spike"
oryxa replay $SID
```

## Flags

Every command takes `-json` for scripting.

| | |
|---|---|
| `-connectors DIR` | connector directory (`ORYXA_CONNECTORS`) |
| `-server URL` | a running Oryxa (`ORYXA_URL`) |
| `-token TOKEN` | API token (`ORYXA_TOKEN`) |

### `serve`

| | |
|---|---|
| `-addr :8080` | listen address |
| `-db DSN` | postgres; in-memory if unset (`ORYXA_DATABASE_URL`) |
| `-token TOKEN` | require this token (`ORYXA_TOKEN`) |
| `-trust-header HEADER` | identity from your proxy (`ORYXA_TRUST_HEADER`) |
| `-summariser AGENT` | roll long context lists up (`ORYXA_SUMMARISER`) |
| `-admin-token TOKEN` | required to add or remove an agent (`ORYXA_ADMIN_TOKEN`) |
| `-room-turns-per-min N` | turns one room may start (default 30, `0` = off) |
| `-turns-per-min N` | turns across the server (default 120, `0` = off) |
| `-allow-private-agents` | API-registered agents may reach private addresses |
| `-reset` | erase the log on start (`ORYXA_RESET`) — development only |

What each of these is for: [running.md](running.md).

### `oryxa-shim`

| | |
|---|---|
| `-addr 127.0.0.1:8090` | listen address |
| `-agents shim.yaml` | agent definitions (`ORYXA_SHIM_AGENTS`) |
| `-token TOKEN` | require this bearer token (`ORYXA_SHIM_TOKEN`) |

Open to anyone who can reach the port if `-token` is empty, and reaching that
port is enough to run what is in the config file.
