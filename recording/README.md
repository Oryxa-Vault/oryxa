# Recording a room

A scripted two-person session against the real stack, recorded, and cut down to
about a minute.

Scripted rather than performed, for one reason: the UI changes, and a demo you
have to act out again by hand is a demo that quietly goes stale. This one is
re-run after a change and the shot lands identically.

```bash
npm install
npx playwright install chromium

node demo.mjs        # drives two browsers, writes ./out and ./final/marks.json
node ramp.mjs        # writes ./final/oryxa-multiplayer-cut.mp4
```

Needs a server already running, with both agents signed in:

```bash
oryxa serve --addr :8099 &
```

| variable | |
|---|---|
| `ORYXA_URL` | default `http://localhost:8099` |
| `ORYXA_TOKEN` | if the server was started with `-token` |
| `ORYXA_AGENTS` | default `claude-code-local,codex-local` |

## Why two browsers

Playwright contexts are the cheapest honest way to have two people in a room:
separate cookie jars, separate streams, separate videos. A single process
posting twice with different author names proves nothing, because the interesting
claim is not that two names appear — it is that two clients watch the same room
change under each other.

It also means the second person **enters the room the way a second person has
to**: with the room's secret, through `/join`, which hands back a cookie scoped
to that room. Anything that breaks read scoping breaks the recording.

## Why the ramp

Agent latency is the proof the demo is real and it is also minutes of a
stationary screen. `ramp.mjs` keeps real time wherever something moves — typing,
sending, the first seconds of the lanes lighting up, the last seconds as an
answer lands — and compresses only the waiting, at 12×. Nothing that moves is
sped up.

The marks come from `demo.mjs` rather than from watching the video, because
"when was an agent thinking" is not recoverable afterwards. A typical run:

```
217s -> 55s (31s real, 24s compressed from 194s of waiting)
```

It stacks the two people **before** trimming. Ramping each video separately lets
them drift apart at every cut, and two panes out of sync would quietly destroy
the one thing the shot exists to prove.

## What is in the take

1. Alice asks the room a question; every agent answers, at once, with the lane
   strip showing both clocks running.
2. Bob — a different browser, watching the same room — names one agent, and only
   that one answers.
3. Alice says thanks, and the room says out loud that nobody needs to answer it.
4. It ends on shared context: two agents from rival labs writing to one list.

Footage is gitignored. The scripts are the artefact; the video is a build output.
