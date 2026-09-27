// Drive headless Chromium over the DevTools protocol for scripts/screenshot:
// open a session through the launch nonce, load a page, capture it.
//
//   node scripts/screenshot.mjs <origin> <nonce> <path> <out.png> <width> <height> <scale>
//
// PROFILE is a throwaway Chromium profile directory.
import { spawn } from 'node:child_process';
import fs from 'node:fs';
const [origin, nonce, path, out, W, H, scale] = process.argv.slice(2);
const port = 9700 + Math.floor(Math.random() * 200);
const chrome = spawn('chromium', ['--headless=new', `--remote-debugging-port=${port}`, `--user-data-dir=${process.env.PROFILE}`, '--no-first-run', 'about:blank'], {
  stdio: ['ignore', 'ignore', process.env.SCREENSHOT_DEBUG ? 'inherit' : 'ignore'],
  // Chromium's singleton socket goes in TMPDIR; a long one overflows a socket path.
  env: { ...process.env, TMPDIR: process.env.XDG_RUNTIME_DIR || '/tmp' },
});
const sleep = ms => new Promise(r => setTimeout(r, ms));
try {
  let ws;
  for (let i = 0; i < 300 && !ws; i++) { try { const t = (await (await fetch(`http://127.0.0.1:${port}/json`)).json()).find(t => t.type === 'page'); if (t) ws = t.webSocketDebuggerUrl; } catch {} await sleep(100); }
  if (!ws) throw new Error('chromium: no DevTools endpoint after 30s');
  const sock = new WebSocket(ws); let id = 0; const pend = new Map();
  sock.onmessage = ({ data }) => { const m = JSON.parse(data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
  await new Promise(r => sock.onopen = r);
  const send = (method, params = {}) => new Promise(r => { const n = ++id; pend.set(n, r); sock.send(JSON.stringify({ id: n, method, params })); });
  await send('Emulation.setDeviceMetricsOverride', { width: +W, height: +H, deviceScaleFactor: +scale, mobile: false });
  await send('Page.enable');
  await send('Page.navigate', { url: `${origin}/open?nonce=${nonce}` });
  await sleep(1500); // the redirect home sets the session cookie
  await send('Page.navigate', { url: `${origin}${path}` });
  await sleep(2500); // the split pane and mail frames fill in after load
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(out, Buffer.from(r.result.data, 'base64'));
  sock.close();
} finally { chrome.kill(); }
