# Oryxa — Integrations

**Date:** 2026-08-05
**Status:** draft for review

How any agent framework connects. The whole answer is two config fields that every framework
already exposes.

---

## 1. The contract

```python
base_url = "https://oryxa.internal/v1/s/proj-alpha"   # ← the session
api_key  = "oryxa_sk_researcher"                      # ← the identity
```

**Session from the URL. Identity from the key.** Nothing else. No SDK, no headers, no
registration, no code.

### 1.1 The URL is the session

Two agents pointed at the same URL are in the same session. That's the entire multiplayer
mechanism.

Putting the session in the *path* rather than a header is deliberate: **every framework lets
you set `base_url`; not every framework lets you set custom headers.** The path is the lowest
common denominator, and it degrades to a string concat when sessions are dynamic — in the same
line of config they were already writing.

### 1.2 The key is the identity

Oryxa holds the real provider credentials and issues its own keys. The user was already going
to replace `api_key` to route through us, so the field is free — it costs them nothing extra to
carry identity.

### 1.3 Be liberal about paths

Frameworks mangle base URLs differently — some append `/v1` themselves, some normalize trailing
slashes, some join paths naively. All of these must resolve to the same session:

```
/v1/s/proj-alpha        /s/proj-alpha        /s/proj-alpha/v1        /v1/s/proj-alpha/v1
```

Cheap to implement, and it removes what would otherwise be the single largest source of "it
doesn't work" reports.

---

## 2. The three named frameworks

### LangGraph / LangChain

```python
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(
    base_url="https://oryxa.internal/v1/s/proj-alpha",
    api_key="oryxa_sk_researcher",
)
```

Cleanest of the three. `ChatAnthropic` takes the same shape once we speak the Anthropic wire
format. Per-node models mean per-node identity comes free if each node's LLM gets its own key —
otherwise §4 handles it.

### CrewAI

**Configure by environment, not just by object.**

```bash
OPENAI_API_BASE=https://oryxa.internal/v1/s/proj-alpha
OPENAI_API_KEY=oryxa_sk_crew
```

CrewAI routes through LiteLLM, and its internal operations — planning, memory, summarization —
can **bypass the `LLM` object and fall back to env vars**. Object-only config therefore captures
the agents' visible calls and silently misses the framework's own.

Those hidden calls matter twice over: they're real spend, and they're often where the crew's
coordination reasoning lives. Missing them means an incomplete session and, later, a wrong
ledger.

Explicit form, for multi-provider setups:

```python
llm = LLM(
    model="openai/<model>",
    base_url="https://oryxa.internal/v1/s/proj-alpha",
    api_base="https://oryxa.internal/v1/s/proj-alpha",   # LiteLLM reads this one
    api_key="oryxa_sk_crew",
)
```

### Google ADK

Via the `LiteLlm` wrapper for OpenAI-compatible:

```python
from google.adk.models.lite_llm import LiteLlm

agent = Agent(
    model=LiteLlm(
        model="openai/<model>",
        api_base="https://oryxa.internal/v1/s/proj-alpha",
        api_key="oryxa_sk_planner",
    ),
)
```

ADK is the least uniform of the three: env-var base_url support is still an
[open issue](https://github.com/google/adk-python/issues/5383), and native Gemini goes through
Google's own wire format rather than an OpenAI-compatible one. So ADK-on-LiteLlm works at M1;
ADK-on-Gemini waits for the Gemini format (§5).

### Others, for free

| Framework | Config |
|---|---|
| OpenAI Agents SDK | `AsyncOpenAI(base_url=..., api_key=...)` |
| Claude Agent SDK | `ANTHROPIC_BASE_URL` + `ANTHROPIC_API_KEY` |
| Mastra / Vercel AI SDK | provider `baseURL` |
| Anything LiteLLM-backed | `api_base` |

---

## 3. The rule this suggests

> **Prefer environment configuration; treat object config as the override.**

Env vars catch the calls that object config misses — a framework's own planning, memory,
summarization, and guardrail calls never pass through the `LLM` object the user constructed.
Those are exactly the calls you'd never notice were missing, which makes this the difference
between a session that looks complete and one that is.

Docs should lead with env vars for every framework and show object config second.

---

## 4. Sub-agent identity

All three named frameworks are **already multi-agent internally** — a CrewAI crew, LangGraph
nodes, ADK sub-agents. They share one API key, so naive integration collapses them into a
single principal and the session shows one participant doing everything. Ironic, and it guts
the product's core view.

**Default: fingerprint the system prompt.** Each sub-agent carries a distinct role — CrewAI's
role/goal/backstory, ADK's instruction, a LangGraph node's prompt. Hashing the normalized system
message yields a stable sub-identity with zero configuration.

Honest limits: two agents with identical prompts collapse into one; one agent with a
prompt-templated variable splits into many. Mitigations, in order:

1. Hash the **stable prefix**, not the whole message, so injected context doesn't fragment identity.
2. Let users **name and merge** fingerprints in the viewer — a one-time cleanup that persists.
3. Accept an explicit override where the framework can carry one (distinct keys per agent, or a
   header where supported).

Getting this wrong is not fatal, but getting it *silently* wrong is — an unlabeled fingerprint
must show up as "unknown agent," never get folded into a neighbor.

---

## 5. Wire formats, in order

Each unlocks a set of frameworks; build in this order.

| # | Format | Unlocks |
|---|---|---|
| 1 | **OpenAI-compatible** | LangGraph, CrewAI, ADK-via-LiteLlm, OpenAI Agents SDK, LiteLLM, Mastra, most of everything |
| 2 | **Anthropic** | Claude Agent SDK, `ChatAnthropic` |
| 3 | **Gemini** | ADK native, `google-genai` |
| 4 | Bedrock / Vertex | enterprise long tail |

Format 1 alone covers the three named frameworks. That's M1's whole target.

---

## 6. Conformance suite

"Any framework integrates easily" is a claim that decays — these libraries change constantly,
and CrewAI's env-var fallback is exactly the kind of behavior that appears in a point release.

So each supported framework gets a conformance test that runs a real agent through Oryxa and
asserts:

1. **Output identity** — the agent's result is byte-identical to running without Oryxa
2. **Capture completeness** — call count through Oryxa equals the provider's own count
3. **Streaming intact** — first-token latency within tolerance, chunk boundaries preserved
4. **Attribution** — sub-agents resolve to distinct principals

Assertion 2 is the one that catches the CrewAI class of bug, and it's the only way to know an
integration is complete rather than merely working. A framework without a passing conformance
run is documented as unverified, not as supported.

---

## 7. Open

1. **Dynamic sessions** — is `base_url` string-concat good enough, or do we need a helper? *Lean:
   concat; a helper is a dependency and this is one line.*
2. **Key per agent vs per crew** — per agent gives clean identity but more setup. *Lean: per
   crew by default, fingerprinting (§4) for the split, per-agent keys as the precise option.*
3. **Non-OpenAI-shaped streaming** — Anthropic and Gemini stream differently; the tap must
   handle each without buffering.
4. **What happens on unknown session id** — auto-create, or reject? *Lean: auto-create. Rejecting
   turns a typo into a failed agent run; auto-create turns it into an empty session someone can
   see and delete.*
