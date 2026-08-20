// A scripted two-person session, recorded.
//
// Two browser contexts are two people: separate cookie jars, separate streams,
// separate videos. That is the claim on the front of the README, and it is the
// one thing a shell driving curl with two different -as flags cannot show — one
// process typing twice is not two people watching each other type.
//
//   node demo.mjs && node ramp.mjs
//
// Needs a running server, and both agents signed in. See README.md.
//
// The timing marks matter as much as the footage. Real agent turns are most of
// the running time and almost none of the interest, and where each one starts
// and stops is not recoverable from the video afterwards — so this writes it
// down while it knows, and ramp.mjs reads it back.

import { chromium } from 'playwright';
import { writeFileSync, mkdirSync } from 'fs';

const BASE = process.env.ORYXA_URL || 'http://localhost:8099';
const TOKEN = process.env.ORYXA_TOKEN || '';
const AGENTS = (process.env.ORYXA_AGENTS || 'claude-code-local,codex-local').split(',');
const SIZE = { width: 1280, height: 900 };

const BEAT = 1400;
const sleep = ms => new Promise(r => setTimeout(r, ms));

let t0 = 0;
const waits = [];
const at = () => (Date.now() - t0) / 1000;

async function api(path, opts = {}) {
  const r = await fetch(BASE + path, {
    ...opts,
    headers: {
      'Content-Type': 'application/json',
      ...(TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {}),
      ...(opts.headers || {}),
    },
  });
  if (!r.ok) throw new Error(`${path} -> ${r.status} ${await r.text()}`);
  return r.json();
}

// enterRoom lets a browser into a room and opens it, through the page's own
// fetch so the HttpOnly cookie lands in that context. This is the path a second
// person actually takes: rooms are entered with a secret, not with a name.
async function enterRoom(page, id, secret, who) {
  await page.goto(BASE);
  if (TOKEN) {
    await page.evaluate(async t => {
      await fetch('/v1/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: t }),
      });
    }, TOKEN);
    await page.reload();
  }
  await page.waitForFunction(() => typeof window.openSession === 'function');
  await page.evaluate(async ([id, secret]) => {
    await fetch(`/v1/sessions/${id}/join`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ secret }),
    });
    await window.openSession(id);
  }, [id, secret]);
  await page.fill('#who', who);
  await page.waitForSelector('#lanes', { state: 'visible' });
}

// say types at human speed. Pasting a sentence instantly reads as automation,
// and the point of the recording is that this is a room with people in it.
async function say(page, text) {
  await page.click('#text');
  await page.type('#text', text, { delay: 45 });
  await sleep(400);
  await page.click('#send');
}

// waitIdle records the span it waited so the ramp can compress it. The cap
// matters: an agent that hangs should not take the recording with it.
async function waitIdle(page, label, capMs = 240000) {
  const start = at();
  const until = Date.now() + capMs;
  while (Date.now() < until) {
    const state = await page.textContent('#rstate').catch(() => '');
    if (state && state.trim() === 'idle') break;
    await sleep(500);
  }
  waits.push({ label, start, end: at() });
}

const run = async () => {
  const room = await api('/v1/sessions', {
    method: 'POST',
    body: JSON.stringify({ agents: AGENTS }),
  }).catch(e => {
    console.error(`\ncould not open a room: ${e.message}`);
    console.error(`is a server running at ${BASE}, with ${AGENTS.join(' and ')} configured?\n`);
    process.exit(1);
  });
  console.log('room', room.id);

  const browser = await chromium.launch();
  const mk = async name => {
    const ctx = await browser.newContext({
      viewport: SIZE,
      recordVideo: { dir: `out/${name}`, size: SIZE },
      colorScheme: 'dark',
    });
    return { ctx, page: await ctx.newPage() };
  };

  const alice = await mk('alice');
  const bob = await mk('bob');
  t0 = Date.now();   // the videos start about here

  await enterRoom(alice.page, room.id, room.secret, 'alice');
  await enterRoom(bob.page, room.id, room.secret, 'bob');
  await sleep(BEAT);

  // 1. One question, every agent, answering at once. The lane strip is the shot:
  //    two clocks running side by side is the whole product in one frame.
  await say(alice.page, 'both of you: what is the biggest risk in this repo right now?');
  await sleep(2500);
  await waitIdle(alice.page, 'both answer');
  await sleep(BEAT);

  // 2. The other person is in the same room, saw all of it, and speaks. One
  //    agent answers, because the message named it.
  await say(bob.page, '@codex which file would you change first?');
  await sleep(1500);
  await waitIdle(bob.page, 'one agent');
  await sleep(BEAT);

  // 3. Politeness costs nothing, and the room says so out loud rather than
  //    quietly waking everybody.
  await say(alice.page, 'thanks, both of you');
  await sleep(3500);

  // 4. End on what they left behind for each other. Two agents from rival labs
  //    writing to one list is the part people do not expect.
  await alice.page.evaluate(() => {
    const p = document.querySelector('.ctxpanel');
    if (p) p.scrollTop = 0;
  });
  await sleep(3000);

  mkdirSync('final', { recursive: true });
  writeFileSync('final/marks.json', JSON.stringify({ room: room.id, waits }, null, 2));
  console.log('waits:', waits.map(w => `${w.label} ${(w.end - w.start).toFixed(0)}s`).join(', '));

  for (const p of [alice, bob]) {
    await p.page.close();
    await p.ctx.close();     // the video is only written on close
  }
  await browser.close();
  console.log('done — footage in ./out, marks in ./final/marks.json');
};

run().catch(e => { console.error(e); process.exit(1); });
