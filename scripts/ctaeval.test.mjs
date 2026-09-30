// scripts/ctaeval.mjs's local server (run by internal/testmail/cmd/ctaeval's
// tests): it serves the page and the shipping scripts, only to its own
// Host, and has no route that reads a body.
import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { createServer } from './ctaeval.mjs';

const get = (port, p, host) => new Promise((resolve, reject) => {
  const req = http.request({ host: '127.0.0.1', port, path: p, headers: host ? { host } : {} }, (res) => {
    let body = '';
    res.on('data', (d) => { body += d; });
    res.on('end', () => resolve({ status: res.statusCode, body }));
  });
  req.on('error', reject);
  req.end();
});

test('the server: the page and the scripts, no body route, its own Host only', async () => {
  const server = createServer();
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  const { port } = server.address();
  try {
    for (const p of ['/', '/eval.js', '/static/actions.js', '/static/mailframe.js', '/static/purify.min.js']) {
      assert.equal((await get(port, p)).status, 200, p);
    }
    const page = await get(port, '/eval.js');
    assert.ok(!/fetch\(/.test(page.body), 'the page fetches nothing: bodies come over the pipe');
    for (const p of ['/body/0', '/body/0000.html', '/bodies/manifest.json', '/manifest.json', '/0000.html', '/profile/', '/static/../../bodies/0000.html', '/static/app.js']) {
      assert.equal((await get(port, p)).status, 404, p);
    }
    for (const host of ['localhost:' + port, 'evil.example', '127.0.0.1', '127.0.0.1:' + (port + 1)]) {
      assert.equal((await get(port, '/static/actions.js', host)).status, 403, host);
    }
  } finally {
    server.close();
  }
});
