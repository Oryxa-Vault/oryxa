# Oryxa — Domain Plan

**Date:** 2026-08-04
**Status:** draft for review
**Depends on:** [research/01-landscape.md](../research/01-landscape.md)

---

## 0. Two reframes that set the architecture

### Reframe A — "low-code" means Oryxa is a *substrate exposed as endpoints*, not a library you build inside

If adopting Oryxa requires restructuring an agent, the product is dead — the pitch is "your
existing agent, now multiplayer." That forces a shape: the core runs as a **service**;
integration is a **thin adapter** per framework; everything Oryxa knows about is
**addressable and streamable**. Polyglot falls out for free — one core, thin clients per
language, and people who never touch our adapters can speak the API directly.

**Design rule:** if a capability isn't reachable over an endpoint, it isn't done.

### Reframe B — A2A is built for opaque strangers; Oryxa's primary case is transparent teammates

A2A's guiding principle is *Opaque Execution*. Correct for cross-org. **Inside a trust
boundary, opacity is a cost, not a feature** — it's what forces context to move by copy and
produces the 36.9% handoff-misalignment failure rate.

> **A2A is a projection of the Oryxa session model, applied at a trust boundary.**

This has a clean structural consequence: **A2A is not a domain.** It's a transport in
Endpoints. Its concepts decompose into domains we already need — trust zones into Identity,
shadow peers into Sessions, constrained rights into Governance. If A2A needed its own domain,
the decomposition would be wrong.

---

## 1. Domains

Nine domains and one cross-cutting layer. Every name is a thing a developer can hold in their
head, and the four **Core** domains are the product promise verbatim.

```
        ┌──────────────────────────────────────────────────┐
 EDGES  │  Adapters  ────────────────────►  Endpoints      │
        │  (frameworks plug in)          (world reaches in) │
        └───────────────────────┬──────────────────────────┘
        ┌───────────────────────┴──────────────────────────┐
 CORE   │  Sessions      Collaboration                      │
        │  Delegation    Continuity                         │
        └───────────────────────┬──────────────────────────┘
        ┌───────────────────────┴──────────────────────────┐
 FOUND. │  Events  (the log — the medium everything shares) │
        │  Identity                                         │
        └───────────────────────┬──────────────────────────┘
        ┌───────────────────────┴──────────────────────────┐
 DERIVED│  Ledger        Replay      (folds over Events)    │
        └──────────────────────────────────────────────────┘

 GOVERNANCE ── cross-cutting: every path above passes through it
```

### Foundation

| Domain | Owns | In one line |
|---|---|---|
| **Events** | Append-only, ordered, hash-chained log. Snapshot + tail. The write path. | *Everything that happens, in order, permanently.* |
| **Identity** | Principals (human / agent / sub-agent), Peer Cards, trust zones | *Who is who, and how much we trust them.* |

Events is not a peer of the others — it's the **medium**. Every other domain is a writer or a
reader of it. Get this interface right first; everything else depends on its shape.

### Core — the promise

| Domain | Owns | In one line |
|---|---|---|
| **Sessions** | The multiplayer container: membership, presence, lifecycle, addressing | *Who's in the room right now.* |
| **Collaboration** | Shared context: regions, isolation levels, ownership, merge & conflict | *How they work on the same thing without corrupting it.* |
| **Delegation** | Grants: scoped views, expiry, revocation, depth limits | *How work is handed down without handing over everything.* |
| **Continuity** | Durable handoff, checkpoint/resume, human-in-loop pause, replay-to-recover | *Nothing is lost when something dies.* |

### Derived — projections only, never authoritative

| Domain | Owns | In one line |
|---|---|---|
| **Ledger** | Provenance chains + resource accounting | *Who did that, and who paid for it.* |
| **Replay** | Time travel, traces, derivation-graph browsing | *Show me exactly how we got here.* |

Both are **folds over Events**. Neither may be written to directly. If a feature request wants
to write to the Ledger, the answer is always an Event.

### Edges

| Domain | Owns | In one line |
|---|---|---|
| **Adapters** | Inbound framework seams: MCP floor / hooks / checkpointer / stream tap | *Your framework, plugged in.* |
| **Endpoints** | Outbound protocols: REST+SSE, A2A (egress & ingress), MCP, AG-UI | *Everything reachable over the wire.* |

Adapters **translate, never decide.** Any adapter containing a policy judgment is a bug.

### Cross-cutting

**Governance** — scope enforcement, approval gates, quotas. Deliberately *not* a domain you
call. The governed-memory study found a real leak because one retrieval path checked tenant
scope but skipped sub-tenant scope — a trust=1 agent read cross-fleet rows. Scope enforcement
fails at exactly the path someone forgot to route through it, so it can't be a callable
service; it has to be a layer nothing can go around.

---

## 2. The names are the API

Because the core is a service and the OSS core must be pleasant to build on, the domain names
become the public surface — modules, endpoints, and event namespaces all at once. If a name
doesn't work in all three places, it's the wrong name.

```
oryxa.events        POST /sessions/:id/events        event:  session.created
oryxa.identity      GET  /principals/:id                     peer.joined
oryxa.sessions      GET  /sessions/:id                       context.region.written
oryxa.collab        GET  /sessions/:id/context/:region       context.ownership.transferred
oryxa.delegation    POST /sessions/:id/grants                grant.issued / grant.revoked
oryxa.continuity    POST /sessions/:id/checkpoints           run.suspended / run.resumed
oryxa.ledger        GET  /sessions/:id/ledger                —  (derived)
oryxa.replay        GET  /sessions/:id/replay?to=:offset     —  (derived)
```

Consistency check that the decomposition is sound: **each SPI belongs to exactly one domain.**

| SPI | Domain | Extend to add | Reference impl in-tree |
|---|---|---|---|
| Adapter | Adapters | a framework seam | LangGraph middleware, Claude Agent SDK hooks, generic MCP floor |
| Region | Collaboration | an isolation/merge semantic | `queue`, `doc`, `set`, `log` |
| Projection | Ledger / Replay | a view over Events | provenance, resource, OTel |
| Transport | Endpoints | a wire protocol | REST+SSE, A2A, MCP, AG-UI |
| Policy | Governance | an authz/quota engine | scope + trust-level default |

**Test of the design:** an outside contributor supports a new agent framework by implementing
**one** SPI and touching zero foundation files.

Deliberately out of core: planners, agent topologies, hosted control plane, UI beyond a
reference viewer.

---

## 3. Deep-dive: Sessions, Collaboration, Delegation

These three carry the multiplayer claim, so they need their primitives named now.

### 3.1 The thesis

**Handoff = transfer of ownership, not transfer of data.**

| | A2A / message-passing | Oryxa |
|---|---|---|
| Unit | Task | Session |
| Verb | *call* an agent | agent *joins* |
| Context | serialized into each message | a **view** onto shared regions |
| Handoff | send artifacts onward | **transfer write-ownership** |
| Cost per hop | 100–500ms serialize/transfer/deserialize/resync | pointer + ACL change |
| Failure mode | context loss in handoff | not expressible — context never moves |

### 3.2 Sessions

**Peer** — a member, not an endpoint being called. Carries identity, trust zone, adapter tier,
and a context view.

**Membership types** — this is where A2A ingress lands:

- *Full peer* — internal, adapter-integrated, may hold ownership
- *Human peer* — a person; same event stream, different rendering
- **Shadow peer** — an external A2A agent. Restricted view, **cannot hold write-ownership**
  (it proposes, an internal peer commits), outputs quarantined and tagged external in
  provenance, resource use metered to the granting principal.

Shadow peers are how one opaque participant doesn't drag the session's guarantees down to the
weakest member. **The trust asymmetry lives in the model, not in the docs.**

### 3.3 Collaboration

Shared context is a distributed-systems problem, not a memory problem — lost updates, dirty
reads, and cascade contamination that poisoned 87% of downstream decisions in four hours. The
answer is **region-typed state**, not one global consistency setting:

| Region type | Semantics | For |
|---|---|---|
| `queue` | serializable — exactly one claimant | work items, task claims |
| `doc` | CRDT merge | co-edited artifacts |
| `set` | read-committed | findings, observations |
| `log` | append-only | decisions, rationale |

Deterministic reducers, never last-write-wins — LWW fails outright under clock skew across
distributed agents. And CRDT is not the universal answer: CRDTs converge on *structure*, not
*meaning*, so two agents can both "win" a merge and produce a coherent document asserting two
contradictory things.

**Ownership** is separate from access: a region has at most one writer-owner at a time, and
`transfer` moves it. That's what makes the thesis in §3.1 safe.

### 3.4 Delegation

A grant is a **capability, not a message**:

```
Grant {
  from: PrincipalRef          # attributable — closes the "who did that?" gap
  to: PrincipalRef
  regions: [RegionRef + mode] # read | write | own
  tools: [ToolRef]
  view: ProjectionSpec        # filtered view of parent context
  expires_at: Timestamp
  revocable: bool             # revocation cascades down the subtree
  depth_limit: int
}
```

`view: ProjectionSpec` is the sleeper. The delegate gets a **filtered projection** — not a copy,
not the whole thing. One mechanism, two wins: least privilege, *and* no context-window collapse
in deep delegation trees. Correctness and cost at the same time.

**Interaction modes:** `address` (peer→peer, logged) · `delegate` (grant + narrowed view) ·
`claim` (serializable, exactly one) · `publish` (event fan-out) · `transfer` (ownership moves).

Conspicuously absent: any "send my context to you."

### 3.5 The line we don't cross

**Oryxa ships delegation primitives, never a planner or supervisor topology.** Holding this
line is what keeps it a substrate instead of multi-agent framework #47 — and it's what makes
"works with any framework" credible, because the frameworks keep owning orchestration.

---

## 4. Open decisions

1. **Session vs Space.** A Session is one episode; a Space holds many sessions plus long-lived
   context. *Lean: ship Sessions only in v1, key Events so a Space can be added above without
   migration.*
2. **Who declares regions** — framework author or app author? *Lean: app author declares;
   Peer Cards declare intent against them; mismatch is a startup error, not a runtime surprise.*
3. **`transfer` distinct from `delegate`?** *Lean: yes — transfer is exclusive and moves,
   delegate is additive and narrows. Collapsing them loses exactly-one-writer.*
4. **Revocation timing** — instant cascade, or at next access? *Lean: instant, since scope must
   hold on every path (see §1, Governance).*
5. **Federated trust zone in v1 or v2?** A2A v1.0.1's extension mechanism is the sanctioned
   path to carry Oryxa semantics between two Oryxa deployments without forking the protocol.
   *Lean: design now, build in v2.*
6. **Core implementation language** — doesn't affect the domain model given the
   service+adapters shape, but does affect the contributor pool. Needs a call before code.
