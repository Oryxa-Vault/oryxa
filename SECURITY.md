# Security

## Current posture — read this before deploying

**Rooms are closed and names can be proved. What is left is that neither is on
by default**, so a server started with no flags and no keys is still a laptop
tool, and one started deliberately is not.

### How much a name is worth

Three ways an author is established, strongest first, and every message records
which one it was — in `source` on `input.submitted`, so a reader never has to
assume they are equivalent.

| `source` | established by | worth |
|---|---|---|
| `trusted` | a proxy in front of Oryxa (`ORYXA_TRUST_HEADER`) | as much as your SSO |
| `key` | a room key bound to that name | unforgeable inside that room |
| `claimed` | somebody typing it | nothing |

**Room keys are the fix for identity, and they keep the line this project drew.**
Oryxa still does not establish who anyone is — `POST /v1/sessions/{id}/keys`
mints a key bound to a name, and presenting it *is* being that person, because
the server stops reading the name from the request. Whoever holds the room
decides that this key is Priya, exactly as they decide who gets in at all. A key
issued for one name cannot be used under another, and it opens no other room.

What that does **not** do: the room secret stays a bearer credential. Whoever
holds it can still claim any name, because they are the root that issues keys —
that is the design, not a hole, and it is why the room secret belongs with whoever
runs the room rather than with everyone in it. Hand out keys, not the secret.

And a room where nobody issued keys behaves exactly as before: every name is
`claimed`, which the log now says out loud instead of leaving you to assume.
### Turn budgets

A turn is an agent doing work — a model call at best, and behind a command-line
connector, minutes of one. Turns are bounded per room and across the server,
default 30 and 120 a minute, `0` for unlimited. Over budget answers 429 with
`Retry-After`.

The unit is the turn rather than the request, because one message can wake seven
agents and cost seven, or wake nobody and cost nothing. The bucket is checked
before a message is accepted and charged for the turns it actually started, so
the wake ladder shows up in the bill: acknowledgements never drain a budget.
They cannot refill one either — a room already over budget refuses everything
until it recovers, because whether a message is free is not knowable until the
ladder has run, and running it means accepting the message.

Keyed by room and by server, never by author. An author name is a string the
caller picks, so a per-author limit would be bypassed by picking another one —
the same reason read scoping is a capability. A limit that looks like a limit
and is not is worse than none, because it gets budgeted for.

### The agent registry

`POST` and `DELETE /v1/agents` change configuration, and everything else here
treats configuration as something that comes from the machine: connectors are
files, and what the shim may run is a file. `-admin-token` requires a second
credential for those two routes, sent as `X-Oryxa-Admin`. Reading the registry
and `check` are not privileged — putting the Test Connection button behind a
credential nobody in the viewer holds would be the wrong trade.

Unset, the registry is open to any token holder and the banner says so.

Removing an agent that an **open** room holds is refused with 409 and the list of
rooms, whether or not an admin token is set: it leaves those rooms with a lane
that can never run a turn and nothing in the room saying why, and there is no
recovery but registering it again. `?force=true` when that is the point. Closed
rooms do not pin an agent — they are history, and history may name something that
no longer exists.

### Read scoping — built

One token no longer opens every room. Each session carries a secret, issued once
when it is created and required on every request that reads or writes it: the
API token says you may talk to this server, and the session secret says which
rooms are yours.

It is a capability rather than a list of names, and that is deliberate. Oryxa has
no accounts and is not going to grow any — it accepts identity, it does not
establish it. Scoping on author names would be scoping on a string the caller
picks, which looks exactly like access control and stops nobody. A secret works
the same whether identity comes from a proxy or from a text box.

- Only a SHA-256 hash is stored, and only the hash reaches the event log — so a
  secret survives neither a database backup nor a room member reading its own
  history.
- A wrong secret and a room that does not exist give the identical 404. Any
  difference between them is an oracle for which rooms exist.
- The secret appears exactly once, in the create response. There is no way to
  ask for it again, which is the property that makes holding it mean anything.
- `POST /v1/sessions/{id}/join` exchanges it for an HttpOnly cookie scoped by
  path to that one room, because `EventSource` cannot send a header and the live
  stream needs authenticating too.

Sessions created before this landed have no secret and can no longer be opened;
the server names them at startup rather than leaving you to find out. Their
history is still in the log.

### Where a registered agent may point

`POST /v1/agents` takes a connector whose `base` this server then fetches, which
makes it a request forgery primitive if left unbounded: anyone holding the token
could read internal services through it, and on a cloud instance the metadata
endpoint that holds the instance's credentials.

**Connectors are trusted by origin, not by address.** A file in `connectors/` is
configuration an operator put on the disk, and pointing at localhost is the
normal case — every verified connector here does it. A spec that arrived over
HTTP may only reach public addresses: loopback, private, link-local, CGNAT,
multicast and unspecified are refused.

The check is in the dialer rather than at registration, because that is the only
place the destination is known for certain. A name that resolves publicly when it
is registered can resolve to `169.254.169.254` by the time it is fetched, and the
connection is made to the address that was checked rather than to the name, so
the second lookup in a rebinding attack never happens. Redirects are covered by
the same guard, and so is `check` — which opens a real connection and would
otherwise still answer "is something listening on this internal host".

`-allow-private-agents` puts it back for deployments that register agents on a
private network deliberately. The startup banner says so every time it is on.

Run it on a laptop, behind a VPN, or behind an authenticating proxy you operate.
Don't put it on the public internet yet.

### If you run `oryxa-shim`

The shim starts processes, which makes it the most sensitive thing in this
repository. Three properties hold it together, and all three are deliberate:

- **What may run comes only from a local file.** Nothing in a request names a
  command, and `GET /v1/agents` does not serve the command lines back. This is
  why exec is not a connector field: connectors are registrable over HTTP at
  `POST /v1/agents`, so a spec that could name a command would make that endpoint
  remote code execution behind one shared token.
- **It binds to loopback by default, and refuses to bind anywhere else without a
  token.** Not a warning — reaching this port is enough to run what is in the
  config file, and a line printed at startup is read once, by someone who has
  already decided to run the command.
- **Both shipped agents can write inside their working directory.** Read-only
  made them worse colleagues than they needed to be: asked what was wrong with
  this repo, Codex found a real crash in the event fan-out and could only report
  it as a suspicion, because `go test` could not create its build directory.

  **They are confined by different mechanisms, and only one of them is a
  sandbox.** Codex runs under `-s workspace-write`, enforced by the OS. Claude
  Code has no sandbox — `dir:` sets the working directory and nothing stops an
  absolute path leaving it, which was verified rather than assumed: with `Write`
  merely allowed, it created a file in `/tmp` without objection. It is confined
  instead by path-patterned permissions, `Edit(./**)` and `Write(./**)`, and by
  a shell narrowed to the project's build and test commands. With those in place
  the same request is refused, and it says so.

  Two consequences worth holding on to. A tool named in `--tools` but absent from
  `--allowedTools` is *denied*, not prompted, in headless — so a missing pattern
  looks like an agent that will not try rather than one that cannot. And the
  patterns are the boundary: widening them to `Write(**)`, or adding a broad
  `Bash(*)`, removes it entirely.

  What it costs either way: anyone who can reach one of these rooms can cause
  writes to that directory. Point it at a checkout you can throw away, not the
  one you are working in.

Treat the shim's host as the trust boundary: it has the credentials and the
repository, and a room member's message is the input that drives it.

When read scoping lands, this section changes and the release notes will say so.

## Reporting a vulnerability

Use **[private security advisories](https://github.com/Oryxa-Vault/oryxa/security/advisories/new)**
on this repository. That keeps the report between us until there's a fix.

Please don't open a public issue for anything exploitable.

What helps in a report:

- What an attacker gets, and what access they need to start
- A reproduction — a connector YAML and a request sequence is ideal
- The commit you tested against

You'll get a first response within a few days. If a report turns out to be one of
the known gaps above, we'll say so plainly rather than treating it as new — but
please report it anyway if you're unsure; a duplicate costs us nothing.

## Scope

In scope: the server, the connector executor, the event log, the API surface, the
embedded viewer.

Out of scope: the agent frameworks Oryxa connects to. If LangGraph or CrewAI has
a vulnerability, report it to them. Connector files in `connectors/` are
configuration — a connector pointing somewhere dangerous is a deployment
question, not a framework bug.

## Supported versions

Pre-1.0. Only `main` is supported. There are no backports.
