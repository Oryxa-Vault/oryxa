# Contributing

The most useful thing you can contribute is **a connector for a framework we
haven't tested**. That's a YAML file, and it makes Oryxa work for everyone using
that framework.

## Adding a connector

1. Start from the closest thing in [`connectors/templates/`](connectors/templates).
2. Point it at your running agent and iterate with `oryxa check <name>` — it runs
   a real turn and tells you what it found.
3. When it's green with no warnings, open a PR with the file and a comment at the
   top saying **which version you verified it against**.

Read [docs/integrating.md](docs/integrating.md) first — the recipes cover
streaming, conversations, reasoning models and typed-event protocols, and the
symptom table covers most of what goes wrong.

**Please don't submit a connector you haven't run.** A file that looks right but
was never executed is worse than no file: it costs the next person an afternoon
proving it wrong. Untested contributions get merged as templates, clearly marked
unverified.

## Working on the core

```bash
go test ./...           # unit + end-to-end
go test -race ./...     # required; the session loop is concurrent
go vet ./... && gofmt -l .
```

Two rules the codebase enforces on itself:

**No framework names outside `connectors/`.** The core knows about `Connector`,
never about ADK or LangGraph. `grep -ri "langgraph\|crewai\|adk" internal/` should
return nothing.

**The log is append-only.** Never coalesce, dedupe or batch writes to it. Derived
views may compact; the log may not. Every one of replay, late-join and audit
breaks quietly if writes get optimised away.

## The lines we hold

These aren't style preferences — they're what makes "works with any framework"
true rather than aspirational. A change that crosses one of them will be asked to
justify itself:

- **We never look inside the agent.** Text is text; everything else is recorded
  whole as opaque activity and never interpreted.
- **We never touch prompts or orchestration.** Multi-agent, planning, routing and
  memory belong to the framework.
- **Derived is derived.** If a feature wants to write to a projection, the answer
  is an event.
- **Capabilities are declared, never assumed.** An agent that can't stream must
  not get a fake streaming UI, and one that can't cancel must not get a dead stop
  button.

## Reporting a bug

Include the connector YAML and the output of `oryxa check <name>`. That single
command answers most of what anyone would ask you next.

If the bug is "my agent's answer is wrong", check the **raw** view in the viewer
first — it shows every chunk exactly as your agent sent it, which usually
distinguishes a connector problem from an agent problem in seconds.

## Licence

Apache 2.0. By contributing you agree your work ships under it.
