# Superseded designs

These describe an architecture Oryxa **does not have**. They are kept because
the reasoning that killed each one is more useful than the conclusion, and
because someone will eventually propose one of them again.

| Doc | What it proposed | Why it died |
|---|---|---|
| [04-stack.md](04-stack.md) | An LLM gateway: agents point `base_url` at Oryxa, which observes every model call | Solved multi-**agent** when the job is multi-**user**. Frameworks already do multi-agent well. |
| [05-modes.md](05-modes.md) | Three engagement modes over that gateway, including an LLM-evaluated trigger for when to go multiplayer | Fell with the gateway. The activation ladder is still a good idea if the premise ever returns. |
| [06-integrations.md](06-integrations.md) | Per-framework adapters configured by `base_url` + API key | Also gateway-era. It did produce the tiering instinct that became connector capabilities. |

The live design is [PLAN.md](../../PLAN.md) and [docs/integrating.md](../../docs/integrating.md).

One correction worth carrying forward: 06 claimed CrewAI's internal calls bypass
the configured LLM object. Tested on crewai 1.15.11, that is **only true with
`planning=True`** — a normal crew honours the object completely, and passing
`planning_llm` fixes the planner. The broad version of that claim was wrong.
