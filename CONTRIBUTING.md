# Contributing

**Your contribution would genuinely be loved here.** Oryxa is early and the
surface area is small, which means a first PR has a real chance of being the
thing that makes it work for a framework nobody has tried yet.

Nothing is too small. A typo fix is a contribution. Telling us a connector
didn't work for you is a contribution. Opening an issue that just says "I tried
this and got lost here" is genuinely useful — it tells us where the docs lie.

## The most useful thing you can do

**Write a connector for a framework we haven't tested.** It's a YAML file — no Go
required — and it makes Oryxa work for everyone using that framework.

1. Copy the closest file in [`connectors/templates/`](connectors/templates).
2. Point it at your running agent and iterate with `oryxa check <name>`. It runs
   a real turn and tells you what it actually got back.
3. When it's green, open a PR with the file and a comment at the top saying
   **which version you verified it against**.

That's the whole process. [docs/integrating.md](docs/integrating.md) has the
recipes if you get stuck — streaming, conversations, reasoning models,
typed-event protocols — and a symptom table covering most of what goes wrong.

One ask: **please run it before you send it.** A connector that looks right but
was never executed costs the next person an afternoon proving it wrong. If you
can't run it, send it anyway and say so in the PR — we'll merge it as a template
marked unverified. That's still worth having.

## Working on the core

```bash
cargo test --all-targets                        # unit + end-to-end
cargo fmt --all --check
cargo clippy --all-targets -- -D warnings
```

`oryxa-shim` and the Go client package are still Go, and have their own:

```bash
go test -race ./...     # required; the session loop is concurrent
go vet ./... && gofmt -l .
```

Two rules the codebase enforces on itself:

**No framework names outside `connectors/`.** The core knows about `Connector`,
never about ADK or LangGraph. `grep -ri "langgraph\|crewai\|adk" src/`
should return nothing outside comments and tests.

**The log is append-only.** Never coalesce, dedupe or batch writes to it. Derived
views may compact; the log may not. Replay, late-join and audit all break quietly
if writes get optimised away.

## The lines we hold

These aren't style preferences — they're what makes "works with any framework"
true rather than aspirational. A change crossing one of them will be asked to
justify itself, and that's a conversation worth having, not a rejection:

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
separates a connector problem from an agent problem in seconds.

## Pull requests

- Branch from `main`, keep the PR focused on one thing.
- `cargo test --all-targets` and `go test -race ./...` green before you open it.
- Explain **why** in the description. The what is in the diff.
- Draft PRs are welcome — open one early if you want a second opinion on an
  approach before you finish it.

We'd rather review a rough PR that's going somewhere than never see it. If
you're unsure whether something fits, open an issue and ask — that costs you
five minutes and can save you a weekend.

## Licence

Apache 2.0. By contributing you agree your work ships under it.
