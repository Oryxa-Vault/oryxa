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

Run it on a laptop, behind a VPN, or behind an authenticating proxy you operate.
Don't put it on the public internet yet.

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
