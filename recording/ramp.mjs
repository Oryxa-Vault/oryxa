// Builds the speed-ramped cut from the recording and its marks.
//
// The tension this resolves: agent latency is the proof the demo is real, and
// it is also six minutes of a stationary screen. So the ramp keeps real time
// where something happens and compresses only the waiting — you watch the lanes
// light up at true speed, the clocks sprint, and the answers land at true speed
// again. Nothing that moves is sped up.
//
//   node ramp.mjs
//
// Reads final/marks.json, writes final/oryxa-multiplayer-cut.mp4.

import { readFileSync, writeFileSync } from 'fs';
import { execFileSync } from 'child_process';
import { readdirSync } from 'fs';

const FAST = 12;      // how much the waiting is compressed
const LEAD = 4.0;     // real seconds kept at the start of a wait: lanes lighting
const TAIL = 4.5;     // real seconds kept at the end: the answer arriving
const FPS = 25;

const marks = JSON.parse(readFileSync('final/marks.json', 'utf8'));
const pick = dir => `out/${dir}/` + readdirSync(`out/${dir}`).find(f => f.endsWith('.webm'));
const [A, B] = [pick('alice'), pick('bob')];

const duration = Number(execFileSync('ffprobe', [
  '-v', 'error', '-show_entries', 'format=duration', '-of', 'csv=p=0', A,
]).toString().trim());

// Turn the waits into a list of segments over the whole timeline. A wait that is
// already short stays real — compressing three seconds buys nothing and reads as
// a glitch.
const segments = [];
let cursor = 0;
for (const w of marks.waits) {
  const start = Math.max(cursor, w.start);
  const end = Math.min(duration, w.end);
  if (end - start < LEAD + TAIL + 3) continue;

  segments.push({ from: cursor, to: start + LEAD, rate: 1 });
  segments.push({ from: start + LEAD, to: end - TAIL, rate: FAST });
  cursor = end - TAIL;
}
segments.push({ from: cursor, to: duration, rate: 1 });

const real = segments.filter(s => s.rate === 1).reduce((n, s) => n + (s.to - s.from), 0);
const fast = segments.filter(s => s.rate !== 1).reduce((n, s) => n + (s.to - s.from) / FAST, 0);
console.log(`${duration.toFixed(0)}s -> ${(real + fast).toFixed(0)}s ` +
  `(${real.toFixed(0)}s real, ${(fast).toFixed(0)}s compressed from ` +
  `${(fast * FAST).toFixed(0)}s of waiting)`);

// One filter graph: stack the two people, then trim that stack into segments and
// concat them. Stacking first means the two videos can never drift apart at a
// cut, which they would if each were ramped separately.
const parts = [];
parts.push('[0:v][1:v]hstack=inputs=2,scale=1920:-2,fps=' + FPS + '[stack]');
parts.push('[stack]split=' + segments.length + segments.map((_, i) => `[s${i}]`).join('') + '');
segments.forEach((s, i) => {
  const pts = s.rate === 1 ? 'PTS-STARTPTS' : `(PTS-STARTPTS)/${s.rate}`;
  parts.push(`[s${i}]trim=start=${s.from.toFixed(3)}:end=${s.to.toFixed(3)},setpts=${pts}[v${i}]`);
});
parts.push(segments.map((_, i) => `[v${i}]`).join('') + `concat=n=${segments.length}:v=1:a=0[out]`);

const filter = parts.join(';');
writeFileSync('final/filter.txt', filter);

execFileSync('ffmpeg', [
  '-y', '-i', A, '-i', B,
  '-filter_complex_script', 'final/filter.txt',
  '-map', '[out]',
  '-c:v', 'libx264', '-crf', '20', '-preset', 'medium', '-pix_fmt', 'yuv420p',
  'final/oryxa-multiplayer-cut.mp4',
], { stdio: ['ignore', 'ignore', 'inherit'] });

console.log('wrote final/oryxa-multiplayer-cut.mp4');
