# Running it for real

The quickstart in the [README](../README.md) has everything off, so it stays two
minutes. None of it should be off anywhere else.

Four things separate a demo from something you can leave running: durability, a
token on the API, an answer to who is acting, and a budget on what a room may
spend.

---

## Durability — `-db`

```bash
oryxa serve -db 'postgres://user:pass@host:5432/oryxa?sslmode=disable'
```

Sessions are a fold over the event log, so a restart replays them — history,
agents and all — rather than losing them. Without `-db` the log is in-memory and
the server says so at startup:

```
  ├─ store       postgres — postgres://oryxa:****@127.0.0.1:5432/oryxa
  ├─ auth        shared token
     recovered 3 session(s) from the log
```

A turn that was *running* when the process died is marked interrupted rather
than re-run: the agent may well have finished it, and doing the work twice is
worse than saying the outcome is unknown. Turns that were only queued are
re-queued.

Agent registrations made over the API are durable too — they go to the log and
are restored on start, attributed to whoever made them.

## The API token — `-token`

```bash
oryxa serve -token "$(openssl rand -hex 32)"
```

Send it as `Authorization: Bearer <token>`, or sign in through the viewer, which
exchanges it for an HttpOnly cookie — `EventSource` cannot set headers and the
live stream needs to authenticate too.

Without it, anyone who can reach the port has full access, and the server says
that at startup.

## Rooms carry their own secret

The API token says you may talk to this server. The room secret says which rooms
are yours. Creating one returns it, once:

```bash
oryxa open claude-code codex
#   s_9a2d0e49
#   secret  72177eac70…
#   someone else joins with: oryxa tail s_9a2d0e49 -secret 72177eac70…
```

Send it as `X-Oryxa-Session`, or let the CLI remember it — rooms opened on this
machine are written to `~/.config/oryxa/rooms.json` with mode 0600, so `send`
and `tail` find it without being told.

Only a hash is kept, so a secret cannot be reissued and a stolen backup carries
no keys. A wrong secret and a room that does not exist answer identically,
because telling them apart would say which rooms are real.

## Names that can be proved — `oryxa key`

```bash
oryxa key s_9a2d0e49 priya
```

A key is bound to a name when it is issued, and the holder cannot use it under
any other. Every message records how its author was established — `trusted`,
`key` or `claimed` — so a name never has to be taken on faith.

Oryxa deliberately does not establish who anyone is. Whoever holds the room
decides that this key is Priya, exactly as they decide who gets in at all.

## Identity from your proxy — `-trust-header`

Oryxa has no accounts. Your deployment already has identity, and duplicating it
here would be the same mistake as duplicating orchestration. Point it at the
header your proxy already sets and the log records people instead of claims:

```bash
oryxa serve -trust-header X-Forwarded-User   # oauth2-proxy, Pomerium,
                                             # Cloudflare Access, ALB, Istio…
```

The body cannot override it, and a request that skipped the proxy is **refused**
rather than treated as anonymous — treating it as anonymous would accept exactly
what the proxy exists to prevent.

> Only safe when nothing can reach the port except that proxy. Bind to localhost
> or a private network; a header is spoofable by anyone who can connect directly.

Unset, authors stay self-declared and the startup banner says so:
`identity  self-declared — the log records claims, not people`.

## Turn budgets

```bash
oryxa serve -room-turns-per-min 30 -turns-per-min 120
```

30 a minute per room and 120 across the server by default, `0` for unlimited.

A turn is an agent doing work, so this is the budget that costs money rather
than a request counter: one message can wake seven agents and cost seven, or
wake nobody and cost nothing. The wake ladder shows up in the bill.

## The agent registry — `-admin-token`

Guards adding and removing agents, sent as `X-Oryxa-Admin`. Reading the registry
and `check` stay open, so the viewer keeps working.

Removing an agent that an open room holds is refused with the rooms named — it
would leave them with a lane that can never run a turn — unless you mean it:
`?force=true`.

**Agents registered over the API may only reach public addresses.** `base` is a
URL this server fetches, so without a line there, `POST /v1/agents` reads any
internal service — and on a cloud host, the metadata endpoint holding its
credentials. Connectors in `connectors/` are unaffected and may point anywhere,
because a file on disk is configuration and pointing at localhost is what every
connector here does. `-allow-private-agents` restores the old behaviour when you
register agents on a private network deliberately.

## A long room stops forgetting — `-summariser`

A list is bounded where it is **rendered**, not where it is stored: twenty items
reach a prompt and the log keeps everything. Left there, a long room forgets
quietly — an agent answering from a partial room sounds exactly as confident as
one answering from a whole one.

```bash
oryxa serve -summariser room-summariser
```

```
findings:
- (30 earlier items, summarised) Checkout returned 503s from 14:02, traced to a
  pool capped at 10 against 40 workers. A rollback was attempted twice.
- the error rate is back under 1 percent
```

The summariser is an ordinary connector, so this is a slot rather than a model
dependency — unconfigured, the count-only marker stays and Oryxa still runs with
no API key. A summary is written **once** and replayed as data: it is a model's
output, and recomputing it would give a different room after every restart.

---

## Docker

```bash
docker build -t oryxa .
docker run -p 8080:8080 -v ./connectors:/connectors oryxa
```

15MB, `scratch`, no cgo — the viewer is embedded, so there is nothing to serve
alongside it. SIGTERM drains rather than killing in-flight turns.

## Endpoints

[`openapi.yaml`](../openapi.yaml) is the contract — every route, every shape,
written against the handlers rather than from memory. A test compares it to the
registered routes in both directions, so it cannot drift into being almost
right.

```
GET    /                                 the viewer
POST   /v1/agents                        register (JSON or YAML)
GET    /v1/agents                        list
POST   /v1/agents/{name}/check           probe with a real turn

POST   /v1/sessions                      {agents} -> session
GET    /v1/sessions/{id}                 state, queue, history
POST   /v1/sessions/{id}/input           {text, author, to?} — returns who it woke
DELETE /v1/sessions/{id}/input/{tid}     withdraw a queued turn
POST   /v1/sessions/{id}/cancel          stop the running turn
POST   /v1/sessions/{id}/close

GET    /v1/sessions/{id}/context         shared state
POST   /v1/sessions/{id}/context/{key}   {append} or {value} (If-Match: version)
POST   /v1/sessions/{id}/context/{key}/pin

GET    /v1/sessions/{id}/stream?since=   SSE, resumable from any point
GET    /v1/sessions/{id}/events?since=   raw log
```

`POST /v1/agents` takes the same connector as JSON, so an agent can be added at
runtime with no restart and no filesystem access — which is how a platform whose
users build agents in a UI would onboard one. `POST /v1/agents/{name}/check`
runs a real turn and returns structured diagnostics, which is the Test
Connection button already written.

## A Go client

[`client/`](../client) is the only non-internal Go package, and the reason is
narrow: it is a thin wrapper over `/v1`, so it is stable exactly as far as `/v1`
is.

```go
import "github.com/Oryxa-Vault/oryxa/client"

c := client.New("http://localhost:8080")
room, _ := c.Open(ctx, "researcher", "critic")

in, _ := c.Say(ctx, room.ID, "priya", "what should we do about the budget")
// in.Wake = ["researcher"], in.Why = "interest: budget"

c.Stream(ctx, room.ID, 0, func(ev client.Event) bool {
    fmt.Println(ev.Kind, ev.Actor)
    return true
})
```

Nothing requires it. The HTTP API is the contract and any language can call it.

## Stability

`/v1` is stable. The connector spec is stable and additive — new fields, never
changed meanings — so a connector written today keeps working.

Everything else stays `internal/` until 1.0: those interfaces are young, and
exporting one is a promise that cannot be withdrawn.

See [SECURITY.md](../SECURITY.md) for the posture as a whole, and what it does
not cover.
