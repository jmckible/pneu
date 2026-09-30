// The browser half of scripts/ctaeval (docs/actions.md, "Evaluating it"):
// render each extracted body through the shipping mailframe.js and
// actions.js in headless Chromium, run the button heuristic, and print one
// JSON line per message with only the fields below.
//
//   node scripts/ctaeval.mjs <dir>
//
// <dir> is the script's private temp dir: <dir>/bodies holds ctaeval's
// manifest.json and bodies, <dir>/profile becomes Chromium's profile.
//
// Bodies never go over HTTP: each is handed to the page over the DevTools
// pipe, as an argument to a function call. The local server serves only
// the page and the shipping scripts, and only to its own Host.
//
// Rendering real mail must never reach the network, so nothing can: remote
// images are off (the frame's CSP has no https:), every request the page
// makes goes through a DevTools request filter that fails anything not for
// the local server, every name resolves to nothing, and every non-loopback
// connection goes to a dead proxy. DevTools is spoken over a pipe, not a
// port, so no other local process can attach to the session.
//
// Chromium runs in its own process group, which is killed on exit, on a
// signal and on an uncaught error, and waited for, so the caller can
// remove the profile.
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

// The page: the frame at the eval's fixed width, the shipping scripts. Its
// own CSP names nothing but itself; a srcdoc frame inherits it on top of
// the frame's own (AGENTS.md), so it is one more wall.
const PAGE = `<!DOCTYPE html><html><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src data:">
<style>body{margin:0} #f iframe.mail{display:block;width:900px;border:0;height:0}</style>
<script src="/static/purify.min.js"></script><script src="/static/mailframe.js"></script>
<script src="/static/actions.js"></script><script src="/eval.js"></script>
</head><body><div id="f"></div></body></html>`;
// evalOne renders a body (the string it is called with) and reports what o
// would do: the declared action's presence, the heuristic's pick, the best
// other destination, the score of the pick or else of the top candidate,
// the top candidate when nothing is picked, and how many links showed.
// Link text is capped at 60 code points and destinations are reduced to
// their host here, in the page, so nothing more leaves it.
const EVAL = `window.evalOne = async function (html) {
  var holder = document.getElementById('f');
  holder.replaceChildren();
  var built = Pneu.renderMailFrame(String(html), {}, location.origin, { remoteImages: false });
  var loaded = new Promise(function (r) { built.frame.addEventListener('load', r, { once: true }); setTimeout(r, 8000); });
  holder.appendChild(built.frame);
  await loaded;
  await new Promise(function (r) { setTimeout(r, 30); });
  var A = Pneu.actions, g = A.guess(built.frame, [location.origin]);
  holder.replaceChildren();
  var host = function (u) { try { return new URL(u).hostname; } catch (e) { return null; } };
  var text = function (s) { return A.cap(String(s || ''), 60).text; };
  var best = g.pick || g.ranked[0] || null;
  return {
    declared: !!built.action,
    pick: g.pick ? { text: text(g.pick.text), host: host(g.pick.url) } : null,
    runnerUp: g.runnerUp ? { text: text(g.runnerUp.text), host: host(g.runnerUp.url), score: g.runnerUp.score } : null,
    score: best ? best.score : null,
    top: !g.pick && best ? { text: text(best.text), host: host(best.url) } : null,
    candidates: g.candidates,
  };
};`;
const STATIC = { '/static/purify.min.js': 'purify.min.js', '/static/mailframe.js': 'mailframe.js', '/static/actions.js': 'actions.js' };

// createServer serves the page, eval.js and the shipping scripts, and
// nothing else: no route reads the temp dir. A request whose Host isn't
// the server's own address (a rebinding page in another browser) gets 403.
export function createServer() {
  const server = http.createServer((req, res) => {
    const a = server.address();
    if (!a || req.headers.host !== `127.0.0.1:${a.port}`) { res.writeHead(403); res.end(); return; }
    if (req.method !== 'GET') { res.writeHead(405); res.end(); return; }
    const u = req.url.split('?')[0];
    const send = (type, body) => { res.writeHead(200, { 'Content-Type': type, 'Cache-Control': 'no-store' }); res.end(body); };
    if (u === '/') return send('text/html; charset=utf-8', PAGE);
    if (u === '/eval.js') return send('text/javascript', EVAL);
    if (Object.hasOwn(STATIC, u)) return send('text/javascript', fs.readFileSync(path.join(repo, 'web/static', STATIC[u])));
    res.writeHead(404);
    res.end();
  });
  return server;
}

const EMPTY = { declared: false, pick: null, runnerUp: null, score: null, top: null, candidates: 0 };

async function main(dir) {
  const bodies = path.join(dir, 'bodies');
  const manifest = JSON.parse(fs.readFileSync(path.join(bodies, 'manifest.json'), 'utf8')) || [];
  const counts = { declared: 0, picked: 0, none: 0, errors: 0 };
  const summary = (blocked) => process.stdout.write(JSON.stringify({ summary: { messages: manifest.length, ...counts, blockedRequests: blocked } }) + '\n');
  if (!manifest.length) { summary(0); return; }

  const server = createServer();
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  const origin = `http://127.0.0.1:${server.address().port}`;

  let id = 0, buf = '';
  const pending = new Map(), listeners = [];
  const chrome = spawn(process.env.CHROMIUM || 'chromium', [
    '--headless=new', '--remote-debugging-pipe', `--user-data-dir=${path.join(dir, 'profile')}`,
    '--no-first-run', '--no-default-browser-check', '--disable-background-networking', '--disable-component-update',
    '--disable-sync', '--disable-default-apps', '--disable-extensions', '--disable-gpu', '--mute-audio',
    '--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1',
    '--proxy-server=http://127.0.0.1:9', '--window-size=1000,800',
    'about:blank',
  ], {
    // Its own process group, so its helpers go with it (killGroup).
    detached: true,
    stdio: ['ignore', 'ignore', process.env.CTAEVAL_DEBUG ? 'inherit' : 'ignore', 'pipe', 'pipe'],
    // Chromium's singleton socket goes in TMPDIR: the private dir, unless
    // its path would overflow a socket path.
    env: { ...process.env, TMPDIR: dir.length < 60 ? dir : process.env.XDG_RUNTIME_DIR || '/tmp' },
  });
  const pgid = chrome.pid;
  // Once the group is seen gone its id is never signalled again: it could
  // be reused.
  let reaped = false;
  const killGroup = () => { if (!reaped) try { process.kill(-pgid, 'SIGKILL'); } catch { reaped = true; } };
  const groupGone = () => { try { process.kill(-pgid, 0); return false; } catch { reaped = true; return true; } };
  process.on('exit', killGroup); // uncaught errors included
  let exited = null;
  chrome.on('error', () => { exited = 'chromium did not start'; });
  chrome.on('exit', (code, sig) => {
    exited = 'chromium exited (' + (sig || code) + ')';
    for (const p of pending.values()) p.reject(new Error(exited));
    pending.clear();
  });
  // stop kills Chromium's group and waits (five seconds at most) until
  // every process in it has gone, so the caller can remove its profile.
  const stop = async () => {
    server.close();
    killGroup();
    for (let i = 0; i < 100 && !groupGone(); i++) await new Promise((r) => setTimeout(r, 50));
  };
  for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.on(sig, () => { stop().then(() => process.exit(130)); });

  // DevTools over the pipe: JSON messages, each ended by a NUL.
  const toChrome = chrome.stdio[3], fromChrome = chrome.stdio[4];
  toChrome.on('error', () => {});
  fromChrome.on('error', () => {});
  fromChrome.on('data', (d) => {
    buf += d.toString('utf8');
    let i;
    while ((i = buf.indexOf('\0')) >= 0) {
      const m = JSON.parse(buf.slice(0, i));
      buf = buf.slice(i + 1);
      if (m.id && pending.has(m.id)) {
        const p = pending.get(m.id);
        pending.delete(m.id);
        if (m.error) p.reject(new Error('devtools error')); else p.resolve(m.result);
      } else if (m.method) listeners.forEach((f) => f(m));
    }
  });
  const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
    if (exited) { reject(new Error(exited)); return; }
    const n = ++id;
    pending.set(n, { resolve, reject });
    toChrome.write(JSON.stringify({ id: n, method, params, sessionId }) + '\0');
  });
  const within = (p, ms, what) => {
    let t;
    const late = new Promise((_, rej) => { t = setTimeout(() => rej(new Error(what + ': timed out')), ms); });
    return Promise.race([p, late]).finally(() => clearTimeout(t));
  };

  let blocked = 0;
  try {
    const { targetId } = await within(send('Target.createTarget', { url: 'about:blank' }), 30000, 'chromium');
    const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
    // The request filter: only the local server, everything else failed.
    listeners.push((m) => {
      if (m.method !== 'Fetch.requestPaused' || m.sessionId !== sessionId) return;
      const ours = m.params.request.url === origin || m.params.request.url.startsWith(origin + '/');
      if (ours) send('Fetch.continueRequest', { requestId: m.params.requestId }, sessionId).catch(() => {});
      else { blocked++; send('Fetch.failRequest', { requestId: m.params.requestId, errorReason: 'BlockedByClient' }, sessionId).catch(() => {}); }
    });
    await send('Fetch.enable', { patterns: [{ urlPattern: '*' }] }, sessionId);
    await send('Page.enable', {}, sessionId);
    const loaded = new Promise((r) => listeners.push((m) => { if (m.method === 'Page.loadEventFired' && m.sessionId === sessionId) r(); }));
    await send('Page.navigate', { url: origin + '/' }, sessionId);
    await within(loaded, 30000, 'eval page');
    // The page's global object: each body is an argument to evalOne on it.
    const { result: global } = await send('Runtime.evaluate', { expression: 'window' }, sessionId);
    for (let i = 0; i < manifest.length; i++) {
      let r;
      try {
        const html = fs.readFileSync(path.join(bodies, manifest[i].file), 'utf8');
        const got = await within(send('Runtime.callFunctionOn', {
          objectId: global.objectId, functionDeclaration: 'function (html) { return evalOne(html); }',
          arguments: [{ value: html }], awaitPromise: true, returnByValue: true,
        }, sessionId), 20000, 'message ' + i);
        if (got.exceptionDetails) throw new Error('page error');
        r = got.result.value;
      } catch (e) {
        // Counted, not described: the error could quote the message.
        if (exited) throw e;
        counts.errors++;
        r = EMPTY;
      }
      counts[r.declared ? 'declared' : r.pick ? 'picked' : 'none']++;
      // Only these fields, in this order.
      const line = { domain: manifest[i].domain, declared: r.declared, pick: r.pick, runnerUp: r.runnerUp, score: r.score, top: r.top, candidates: r.candidates };
      process.stdout.write(JSON.stringify(line) + '\n');
    }
    summary(blocked);
  } finally {
    await stop();
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [dir] = process.argv.slice(2);
  if (!dir) { console.error('usage: node scripts/ctaeval.mjs <dir>'); process.exit(2); }
  try {
    await main(dir);
  } catch (e) {
    // A category, never the error itself: it could quote a message.
    console.error('ctaeval: ' + (/^chromium /.test(e && e.message) ? e.message : 'the browser run failed'));
    process.exitCode = 1;
  }
}
