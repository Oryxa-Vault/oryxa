# Oryxa — Stack & Integration Model

**Date:** 2026-08-05
**Status:** decided (core language, integration model), draft (rest)
**Supersedes:** the adapter ladder, the MCP-surface plan, and the prompt-injection gateway in
earlier drafts

---

## 1. The split: their input, our output

The agent stays exactly as it is. **Its author owns everything on the way in** — prompt
assembly, context selection, memory, retrieval, tuning. We don't touch it.

**Oryxa owns what comes out** — capturing it, structuring it, attributing it, chaining it, and
making it available to everyone else in the session.

```
                            ┌──────── observe only, never modify ────────┐
                            │                                            │
┌─────────────────┐   base_url    ┌──────────────────┐   byte-identical   ┌──────────┐
│  Agent          │ ────────────► │  Oryxa gateway   │ ─────────────────► │ Provider │
│  (any framework,│               │                  │ ◄───────────────── │          │
│   unmodified)   │ ◄──────────── │                  │        tap         └──────────┘
└─────────────────┘   untouched   └────────┬─────────┘
                                           │ output → events
                                           ▼
                                  ┌──────────────────┐
                                  │ sessions/context │ ◄──── humans, other agents
                                  │ ledger / log     │        (pull: REST · SSE)
                                  └──────────────────┘
```

**Requests are forwarded byte-identical.** Not "mostly unchanged" — identical, so provider
prompt caching keeps working exactly as it did. This is an implementation invariant, not a
goal.

### 1.1 What that buys

The previous draft rewrote prompts. Everything expensive about that is now gone:

| Risk in the injection design | Status |
|---|---|
| Prompt-cache invalidation multiplying cost, silently | **gone** — byte-identical forwarding |
| Prompt surgery disturbing tuned agents | **gone** — we don't write |
| "What is the proxy doing to my prompts?" | **gone** — provably nothing, and it's Apache-2.0 |
| Injection strategy as the highest-risk design task | **gone** — there is no injection |

"We never modify your prompts" is the strongest possible answer to the only real objection to
routing traffic through a proxy. Worth treating as a product guarantee, not an implementation
detail.

### 1.2 What we still get from the input side

We **read** the request even though we never change it, and that's where provenance comes from.
We see exactly what context produced each output, so `caused_by` is **exact rather than
inferred** — the upgrade over §2.2 of the v1 doc survives intact. Observation was doing that
work all along; modification was never what earned it.

### 1.3 What we capture on the way out

Output arrives already structured, because agents emit structured things:

```
completion.produced   assistant message
toolcall.made         the tool invocations the agent chose  ← structured, semantic, free
toolresult.observed   what came back
usage.recorded        tokens / cost, exact
```

Tool calls are the good part. We don't inject tools; we **watch the ones the agent already
makes**. An agent calling `search(query)` and then `write_file(path, content)` gives us a
semantically rich, ordered, attributed record with nothing added to its prompt.

### 1.4 How output becomes shared context

- **Default: append to the session's `log` region.** Zero config, always correct.
  This is the region type that's conflict-free by construction — the same reason it was already
  the recommended default in v1. The architecture converged on it twice, which is a good sign.
- **Optional: routing rules** map particular tool calls or output shapes into `state` regions
  when someone wants structure.

### 1.5 The honest asymmetry

**Writes are zero-code. Reads are your call.**

We make every output available — live over SSE, queryable over REST, replayable from the log.
We do *not* decide what another agent should know, because deciding that means touching its
input, and its input is not ours.

So agent B sees agent A's work when B's author feeds it in — a small change in their prompt
assembly, made in their own code, where they already have control. That's the same boundary as
"they handle input," applied consistently rather than abandoned when it's inconvenient.

The principle: **Oryxa doesn't decide what an agent should know. It makes everything knowable.**

---

## 2. What this still costs

**Routing model traffic is a real ask**, even read-only. Self-hosting stays mandatory —
`docker run oryxa` in your own VPC is an easy yes, a hosted endpoint is an easy no. The
Apache-2.0 single-binary decision and this architecture hold each other up.

**Streaming must pass through transparently.** A proxy that buffers token streams is unusable.
The tap has to observe the stream while forwarding it, not after.

**Provider coverage.** OpenAI-compatible covers most frameworks; Anthropic second;
Bedrock/Vertex are their own shims. Frameworks with hardcoded endpoints are the long tail.

That's the whole list now. The previous draft's risk section was four times this.

---

## 3. Core: Go

| | |
|---|---|
| **Language** | Go |
| **Binary** | single static, no cgo |
| **Storage** | SQLite (default) → Postgres (production), one interface |
| **Oryxa API** | REST + SSE |
| **Gateway** | transparent streaming proxy + output tap |

1. **Single static binary.** Self-hosting is mandatory (§2), so install friction is a
   first-order product concern.
2. **The core invariant is a language primitive.** A goroutine per session with a channel inbox
   *is* the single serialization point.
3. **Streaming proxies are what Go is for.** And a pass-through tap is far simpler to build
   correctly than a rewriter — this design is a few hundred lines where the last one was a
   subsystem.

**Plugin objection dissolves** — no per-framework adapters exist under this model, so the
Adapter SPI is gone. What remains (Region, Transport, Projection, Policy) is low-volume in-tree
registration.

### 3.1 Dependencies

| Need | Choice | Note |
|---|---|---|
| HTTP | stdlib `net/http` | Go 1.22+ routing suffices. No framework. |
| SQLite | **`modernc.org/sqlite`** (pure Go) | **Not `mattn/go-sqlite3`** — cgo breaks static linking and cross-compilation. The single-binary promise dies on that one import. |
| Postgres | `pgx` | |
| JSON Patch | RFC-6902 lib | context deltas |

### 3.2 Layout

```
cmd/oryxa/
internal/
  gateway/            # transparent proxy + output tap
  events/             # log: append, seq, hash chain
  sessions/           # membership, presence
  sharedctx/          # regions, OCC, views     ← `context` collides with stdlib
  ledger/             # provenance + usage projections
  store/              # sqlite + postgres behind one interface
  api/                # REST + SSE
pkg/oryxa/            # public Go client
```

---

## 4. Build order

| # | Ship | Demo |
|---|---|---|
| 1 | **Gateway pass-through** — proxy, streaming, byte-identical forwarding | unmodified agent runs through Oryxa; output is identical, latency negligible |
| 2 | **Event log** — schema, seq, hash chain | every call captured, attributed, chained; `verify` passes |
| 3 | **Sessions** — binding, members, presence | two agents on one session; both streams land in one place |
| 4 | **Ledger** — usage rollup, provenance walk | exact spend per agent; "what produced this output" returns the chain |
| 5 | **Context** — `log` region, regions, OCC, views | outputs queryable as shared context |
| 6 | **Stream** — SSE, deltas, late-joiner catch-up | a human watches two agents work, live |
| 7 | **Read API + SDK** | agent B's author pulls A's findings into their own prompt |

Step 1 moved to first: it's now small, it's the trust demo, and nothing else is reachable
without it. **Step 4 is the first thing anyone will pay attention to** — exact cross-agent spend
is felt immediately, where correctness is invisible until it fails. **Step 7 is where it becomes
multiplayer.**

---

## 5. Still open

1. **Session binding** — how does a request map to a session? Header, API-key mapping, or URL
   path. Needed before the gateway is real; probably the next thing to decide.
2. **Output routing rules** — what maps a tool call into a `state` region? Config shape TBD.
   Replaces injection strategy as the main design task, at a fraction of the risk.
3. **Session vs Space** — unchanged, still the only migration-class decision.
4. **Provider coverage order** — OpenAI-compatible, then Anthropic.
5. **Auth** — session-scoped bearer tokens for v1.
6. **Payload retention** — commitments allow eviction with the chain intact; policy TBD.
