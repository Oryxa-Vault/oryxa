# Oryxa — Landscape Research

**Date:** 2026-08-04
**Question:** What layer does a framework occupy if its job is to take any single-agent
framework and make it multiplayer end-to-end — shared sessions, shared context, delegation,
durable handoff, events, ledger?

---

## 1. The stack as it actually exists in Aug 2026

| Layer | Concern | Owned by | Status |
|---|---|---|---|
| L1 | Agent → tools/data | **MCP** | Settled. De facto standard. |
| L2 | Agent → agent transport | **A2A** v1.0 (Linux Foundation) | Settled enough. 50+ partners. |
| L3 | Agent → human UI | **AG-UI** (CopilotKit) | Emerging. Shared JSON state + RFC-6902 deltas. |
| L4 | Crash-safe execution | Temporal / Restate / DBOS / CF Workflows | Mature, but agent-blind. |
| **L5** | **Shared session, shared context, concurrency, delegation, ledger** | **— nobody —** | **Open.** |

L5 is the gap. It is not a gap by accident; it is a gap by explicit exclusion.

### The A2A non-goals are, almost verbatim, the Oryxa spec

From the [A2A specification](https://a2a-protocol.org/latest/specification/), out of scope:

- shared state between agents
- agent memory / knowledge persistence
- orchestration or workflow composition
- concurrency control mechanisms
- **billing, ledger, or cost tracking**
- internal agent tool implementations or "thoughts"

A2A's guiding principle is **Opaque Execution** — agents collaborate "without needing access
to each other's internal state, memory, or tools."

> **Design tension to resolve early.** A2A is deliberately opaque. Oryxa's premise is
> deliberate *selective transparency*. These are not in conflict if Oryxa is positioned as
> the layer that **decides what is shared and what stays opaque**, and speaks A2A at the
> trust boundary (cross-org) while sharing substrate inside it (intra-team). Oryxa is not an
> A2A competitor — it is the thing A2A said it wasn't going to be.

A2A does give us one useful primitive to align with: `contextId`, a server-generated
grouping ID for related tasks/messages. It is the closest existing thing to a session — but
it is opaque, server-local, and carries no state, no membership, no ordering guarantees.

---

## 2. Why this layer matters: the failure data

Production postmortems ([Augment Code](https://www.augmentcode.com/guides/why-multi-agent-llm-systems-fail-and-how-to-fix-them),
[Maxim](https://www.getmaxim.ai/articles/multi-agent-system-reliability-failure-patterns-root-causes-and-production-validation-strategies/)):

- **41.8%** of failures — specification & system design (ambiguous roles, bad decomposition,
  missing termination conditions)
- **36.9%** — inter-agent misalignment: **context loss during handoff**, conflicting outputs,
  format mismatch

Coordination cost is the second killer: **100–500ms per handoff** from
serialize → transfer → deserialize → re-sync. A 10-handoff workflow burns **1–5s** of pure
overhead before any work happens. And failure surface grows quadratically — a 4-agent
pipeline has 6 interaction points, a 10-agent pipeline has 45.

> **This is the strongest architectural argument for the whole project.** Message-passing
> multi-agent systems pay the handoff tax on *every* hop because context moves by **copy**.
> A shared-substrate model moves context by **reference** — the handoff cost collapses toward
> the cost of a pointer, and "context loss during handoff" stops being expressible, because
> there is no handoff of context, only a change of who is writing.

Delegation-by-reference instead of delegation-by-copy is the thesis. Everything else in the
design should be judged against whether it protects that property.

---

## 3. The hard part: shared context is a distributed-systems problem, not a memory problem

This is where the research is loudest, and where a naive design will die.

### Silent corruption ([TianPan, Apr 2026](https://tianpan.co/blog/2026-04-20-parallel-agent-shared-memory-contention))

Three documented failure modes when agents concurrently touch shared state:

1. **Lost update** — A reads 100, writes 95; B reads 100, writes 150. Final: 150. A's work is
   gone, silently.
2. **Dirty read** — agent reads a partially-applied mutation, sees state violating invariants.
3. **Cascade contamination** — one corrupted value **poisoned 87% of downstream decisions
   within four hours.**

The lethal property: the resulting state is *syntactically valid and semantically wrong*. It
surfaces as a model failure. Teams will blame the LLM and never find the bug.

Prescribed mitigations, all of which are architecture, not prompt:
- **Per-region isolation levels**, not one global consistency setting. A shared task queue
  needs serializable (exactly one claimant). A findings repository needs only read-committed.
  A design doc needs co-edit merge.
- **Deterministic reducer functions instead of last-write-wins.** LWW fails outright under
  clock skew across distributed agents.

### Governed shared memory ([arXiv 2606.24535](https://arxiv.org/html/2606.24535v1))

Formalizes fleet-memory as `F = (A, M, G, P, T)` — agents, memory, governance, provenance,
temporal ordering. Empirically measured, which makes it unusually useful:

| Mechanism | Result |
|---|---|
| Four nested scopes (agent-local / team-shared / tenant-global / restricted) + trust levels 0–3 | Cross-fleet leakage **0/80** probes after fix |
| Scope enforcement bug found mid-study | A `GET /memories/{id}` path checked tenant scope but **not** sub-tenant scope — trust=1 agent read cross-fleet rows. **Lesson: scope must be enforced on _every_ retrieval path, not just search.** |
| Write-to-visible latency (strong mode) | p50 **0.83s**, p95 **1.63s** |
| Contradiction supersession (`supersedes_id`, older row marked non-active) | 100% when both writes admitted — but **dedup rejected 206/400 writes pre-commit**, dropping end-to-end detection to **49%** |
| Provenance chain reconstruction, depth 4 | **50/50** complete, 100% writer identity preserved, p50 291ms per hop |

> Two takeaways worth stealing directly: (a) **provenance chains are cheap enough to walk
> interactively** (~291ms/hop) — an audit UI over a derivation graph is realistic, not
> aspirational; (b) **the dedup-vs-contradiction interaction is a real trap** — an
> optimization at the write path silently defeated the correctness mechanism downstream.
> Guard against this class explicitly in the event pipeline.

CRDTs ([Zylos](https://zylos.ai/research/2026-03-17-crdts-distributed-state-sync-multi-agent-systems/))
are the right tool for *some* regions — no locks, no leader, guaranteed convergence — with the
emerging pattern being **CRDTs handle convergence mechanics, an LLM arbiter handles semantic
conflict**. But CRDTs converge on *structure*, not *meaning*: two agents can both "win" a
merge and produce a coherent document that says two contradictory things. Region-typed state
is the answer, not CRDT-everywhere.

---

## 4. The ledger — two distinct things sharing a name

Worth splitting now, before the word overloads.

**(a) Provenance ledger — "who did that?"**
The [attribution gap](https://zylos.ai/research/2026-04-25-agent-identity-provenance-signed-audit-trails/):
audit logs record *the user or service that ran the operation*, not *the agent that produced
the upstream data it acted on*. In a delegation chain that is the only question that matters,
and today it is unanswerable.
[VIL / auditable-agents work](https://arxiv.org/html/2604.05485) shows the shape: cryptographically
linked records storing **commitments rather than raw content** — tamper-evident, forensically
reconstructable, and explicitly **not requiring a blockchain**. Any post-hoc edit breaks the
hash chain.

**(b) Resource ledger — "who spent that?"**
Tokens, cost, tool invocations, rate-limit budget — attributed per participant, per session,
per delegation subtree. Nobody owns this. A2A explicitly disclaims it. It's the thing that
makes a multiplayer session financially governable, and it's a genuinely underrated wedge:
it's the feature a buyer can *feel* on day one, whereas correctness guarantees are invisible
until they fail.

Both are **projections of the same append-only event log**, not separate stores. That's the
unifying move: one log, many derived views.

---

## 5. Event log as the spine

If the event log is the source of truth, several requested features stop being features and
become consequences:

| Requirement | Falls out of the log |
|---|---|
| Durable handoff | replay from last committed offset |
| Shared session | subscribe to the session's stream |
| Late joiner (human or agent) | snapshot + tail |
| Audit / provenance | the log *is* the audit trail (hash-chain it) |
| Ledger | fold over the log |
| Time travel / debugging | replay to offset N |
| Observability | tap the stream |

This is the highest-leverage decision in the design, and it should be made first, because
every other component's API depends on whether it reads the log or is authoritative itself.

Reference implementations of the pattern:
[Cloudflare Agents SDK](https://developers.cloudflare.com/agents/) — each agent is a Durable
Object with its own SQLite, WebSocket state sync to all connected clients, hibernation,
resumable streams. Architecturally the closest existing thing to what we want — but it is a
framework you *build inside*, and it is Cloudflare-locked. Oryxa's differentiator is wrapping
frameworks you already run, anywhere.

---

## 6. The "any single-agent framework" claim — feasibility

This is the load-bearing claim of the pitch, so it needs real seams. Surveying what actually
exists to hook into:

| Seam | Coverage | Fidelity | Notes |
|---|---|---|---|
| **MCP server injection** | Universal (2026 de facto standard, incl. OpenAI Agents SDK native) | Medium | Inject Oryxa capabilities *as tools*. Works everywhere, needs zero framework support. Strongest lowest-common-denominator. |
| **Hooks / callbacks** | Claude Agent SDK (`PreToolUse`, `PostToolUse`, `SessionStart`, `SessionEnd`, `Stop`, `UserPromptSubmit`); LangGraph middleware; CrewAI callbacks | High | Best fidelity where available. Per-framework adapter. |
| **Checkpointer / session store** | LangGraph (Postgres+Redis, time-travel), OpenAI Agents SDK v0.13 session persistence, Claude Agent SDK (JSONL on disk) | High | Lets Oryxa own durability without reimplementing the agent loop. |
| **Stream tap (AG-UI)** | Adapters already exist for several frameworks | Medium-High | Free multiplayer *observation*; shared state deltas already RFC-6902. |
| **A2A endpoint** | Any A2A-speaking agent | Low (opaque by design) | Boundary/interop case, not the intra-team case. |
| **Model-call proxy** | Universal | Low semantic level | Fallback for frameworks with no seams at all. Sees tokens, not intent. |

**Conclusion: the claim is defensible**, via a tiered adapter model — MCP injection as the
universal floor, native hooks for high-fidelity integration, graceful degradation between.
Not "any framework equally well," but "any framework at some useful tier," which is the
honest and still-compelling version.

> The tiering should be explicit and user-visible from day one. A framework at MCP-only tier
> cannot offer the same correctness guarantees as one with hooks — pretending otherwise is
> how a substrate loses trust the first time someone's state silently diverges.

---

## 7. Prior art — who is near this, and why none of them close it

| System | What it has | What it lacks for L5 |
|---|---|---|
| **Cloudflare Agents SDK** | Stateful, durable, WebSocket state sync, multiplayer chat/collab, DO-to-DO RPC | Build-inside framework; platform-locked; no ledger; no cross-framework wrapping |
| **Solace Agent Mesh** | Event-driven multi-agent, orchestrator-mediated dispatch, enterprise runtime | Orchestration platform, not a shared substrate; broker-centric; central orchestrator is the coordination model |
| **AG-UI / CopilotKit** | Shared JSON state, JSON-Patch deltas, real-time | Shaped for 1 human ↔ 1 agent; no N↔M membership, no delegation, no ledger |
| **LiveKit** | Rooms, participants, agents as first-class participants, sub-300ms | Media plane; orchestration is explicitly "application layer"; no context governance, no ledger |
| **Temporal / Restate / DBOS** | Real durability, exactly-once, replay | Agent-blind. No session, no sharing, no attribution. Candidate *dependency*, not competitor. |
| **LangGraph** | Graph state, checkpointing, `interrupt()` HITL, time travel | Single-process graph; state is the graph's, not a shared multiparty session |
| **A2A** | Transport, discovery, task lifecycle | Everything in §1, by explicit non-goal |

The white space is real and it's specifically: **N humans + M agents + shared governed state
+ attributed durable history, over frameworks you didn't write.**

---

## 8. Open design questions for the next phase

Ordered by how much downstream design they determine:

1. **Is the event log authoritative, or a projection?** Everything depends on this. (Lean:
   authoritative — §5.)
2. **What is the unit of membership?** Session / room / space — and what's the identity model
   for humans vs agents vs delegated sub-agents? Trust levels 0–3 as in §3, or capabilities?
3. **Region-typed shared context** — what's the taxonomy? (queue → serializable; document →
   CRDT; findings → read-committed; decisions → append-only). Who declares the region type,
   the framework author or the app author?
4. **Delegation semantics** — is a delegation a *message*, a *capability grant*, or a
   *sub-session*? Revocable? Time-bounded? Does the delegate inherit the parent's context
   view or a filtered projection? (Filtered projection is the interesting answer — it's how
   you get least-privilege *and* avoid context-window collapse simultaneously.)
5. **Where does durability come from** — build on Temporal/DBOS/DO, or own it? Owning it is
   more work but avoids forcing an infra dependency on adopters, which matters a lot for a
   "wrap what you already have" product.
6. **Conflict escalation path** — when the deterministic reducer can't resolve, who arbitrates?
   LLM arbiter, human, or fail-closed? (Fail-closed is probably right for v1; silent wrong
   answers are the exact failure mode we're selling against.)
7. **What's the minimum viable tier?** Which single framework do we make excellent first, to
   prove the model, before claiming universality?

---

## Sources

- [A2A Protocol Specification](https://a2a-protocol.org/latest/specification/) · [A2A docs](https://a2a-protocol.org/latest/)
- [Governed Shared Memory for Multi-Agent LLM Systems (arXiv 2606.24535)](https://arxiv.org/html/2606.24535v1)
- [The Silent Corruption Problem in Parallel Agent Systems](https://tianpan.co/blog/2026-04-20-parallel-agent-shared-memory-contention)
- [CRDTs and Distributed State Synchronization for Multi-Agent AI Systems](https://zylos.ai/research/2026-03-17-crdts-distributed-state-sync-multi-agent-systems/)
- [Agent Identity and Signed Provenance](https://zylos.ai/research/2026-04-25-agent-identity-provenance-signed-audit-trails/) · [Auditable Agents (arXiv 2604.05485)](https://arxiv.org/html/2604.05485)
- [From Agent Traces to Trust: Evidence Tracing and Execution Provenance (arXiv 2606.04990)](https://arxiv.org/abs/2606.04990)
- [Why Multi-Agent LLM Systems Fail](https://www.augmentcode.com/guides/why-multi-agent-llm-systems-fail-and-how-to-fix-them) · [Multi-Agent System Reliability](https://www.getmaxim.ai/articles/multi-agent-system-reliability-failure-patterns-root-causes-and-production-validation-strategies/)
- [AG-UI State Management](https://docs.ag-ui.com/concepts/state)
- [Cloudflare Agents docs](https://developers.cloudflare.com/agents/) · [cloudflare/agents](https://github.com/cloudflare/agents)
- [Solace Agent Mesh](https://docs.solace.com/Agent-Mesh/agent-mesh.htm)
- [Durable Execution Patterns for AI Agents](https://zylos.ai/research/2026-02-17-durable-execution-ai-agents/) · [Durable Execution for Agent Runtimes](https://zylos.ai/research/2026-04-24-durable-execution-agent-runtimes/)
- [Multi-Agent in Production 2026: What Actually Survived](https://medium.com/@Micheal-Lanham/multi-agent-in-production-in-2026-what-actually-survived-f86de8bb1cd1)
- [A Survey of Agent Interoperability Protocols (arXiv 2505.02279)](https://arxiv.org/pdf/2505.02279) · [Governance Gaps in Agent Interoperability Protocols (arXiv 2606.31498)](https://arxiv.org/pdf/2606.31498)
