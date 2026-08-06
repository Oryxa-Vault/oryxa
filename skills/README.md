# Agent skills

Drop these into an agent that supports skills (Claude Code reads
`.claude/skills/`, so symlink or copy them there) and it can drive Oryxa itself.

| Skill | For |
|---|---|
| [oryxa-connector](oryxa-connector/SKILL.md) | writing and debugging a connector so a framework can join a room |
| [oryxa-room](oryxa-room/SKILL.md) | driving a room over HTTP — sessions, input, streams, shared context |

**`oryxa-connector` is the one worth having.** Writing a connector is an
iterative loop — probe the agent, read what came back, adjust a selector, probe
again — which is exactly what an agent is good at and what a human finds
tedious. `oryxa check` runs a real turn and names the failure, so the loop
converges in two or three passes instead of an afternoon reading API docs.

```bash
mkdir -p ~/.claude/skills
ln -s "$PWD/skills/oryxa-connector" ~/.claude/skills/oryxa-connector
ln -s "$PWD/skills/oryxa-room"      ~/.claude/skills/oryxa-room
```

Then: *"connect my LangGraph agent on :2024 to Oryxa"*.
