# Security

## Current posture — read this before deploying

**Oryxa is not ready to be exposed to a network you don't control.** This isn't a
disclaimer; it's a specific, known gap:

- **One token opens every session.** There is no participant concept anywhere in
  the codebase. Anyone holding the token can read any room, including rooms they
  were never part of. Read scoping is the largest open item in
  [PLAN.md](PLAN.md).
- **Identity comes from the edge.** Oryxa trusts an author header supplied by a
  proxy in front of it (`ORYXA_TRUST_HEADER`). Without that proxy, callers assert
  their own identity.

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
- **The shipped tool allowlist is read-only.** An allowlist is a stronger
  boundary than a permission mode because it cannot be prompted around. Widening
  it means anyone who can reach the room can change files on that machine —
  which, without read scoping, means anyone holding the token.

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
