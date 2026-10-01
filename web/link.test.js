// node --test web/link.test.js — client mode's link in the page
// (web/static/link.js): the status line's link-down state, the Pneu-Link
// toasts, the details rows. Server mode has no link: all of it says nothing.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const L = require('./static/link.js');

const headers = (h) => ({ get: (k) => (k in h ? h[k] : null) });
const rev = '0123456789abcdef0123456789abcdef01234567';
const down = { state: 'down', since: '2026-09-30T10:00:00Z', reason: 'node-offline', server: 'server', revision: { client: rev, server: '' } };
const up = { state: 'up', since: '2026-09-30T10:00:00Z', reason: null, server: 'server', revision: { client: rev, server: rev } };

test('lineState: link down is its own state, naming the server', () => {
  assert.deepEqual(L.lineState(down), { state: 'link', text: "Can't reach server · retrying" });
  assert.equal(L.lineState(up), null);
  assert.equal(L.lineState({ ...down, state: 'starting' }), null);
  // A server: no link at all.
  assert.equal(L.lineState(null), null);
  assert.equal(L.lineState(undefined), null);
  assert.equal(L.lineState({ state: 'weird' }), null);
  assert.equal(L.lineState({ state: 'down' }).text, "Can't reach the server · retrying");
});

test('outcome: from Pneu-Link, or the JSON body, else nothing', () => {
  assert.equal(L.outcome(headers({ 'Pneu-Link': 'not-sent' }), null), 'not-sent');
  assert.equal(L.outcome(headers({ 'Pneu-Link': 'unknown' }), { ok: false }), 'unknown');
  assert.equal(L.outcome(headers({}), { ok: false, error: 'x', link: 'not-sent' }), 'not-sent');
  assert.equal(L.outcome(headers({ 'Pneu-Link': 'maybe' }), { link: 'other' }), '');
  // The server's own refusal (a 409, a 503): no link outcome.
  assert.equal(L.outcome(headers({}), { ok: false, error: 'locked' }), '');
  assert.equal(L.outcome(null, null), '');
});

test('failText: not sent is safe to retry; unknown waits for the next hello', () => {
  assert.equal(L.failText('not-sent', down), "Not sent: can't reach server.");
  assert.equal(L.failText('unknown', down), "server didn't answer; checking when it's back.");
  assert.equal(L.failText('', down), null);
  assert.equal(L.failText('not-sent', null), "Not sent: can't reach the server.");
});

test('details: the link and both builds, or nothing on a server', () => {
  assert.deepEqual(L.details(up), [
    ['server', 'connected since 2026-09-30T10:00:00Z'],
    ['this build', '0123456789ab'],
    ["server's build", '0123456789ab'],
  ]);
  assert.deepEqual(L.details(down)[0], ['server', 'server is offline since 2026-09-30T10:00:00Z']);
  assert.equal(L.details(down)[2][1], 'unknown');
  assert.deepEqual(L.details(null), []);
  // Only a 40-hex revision shows; anything else is "unknown".
  assert.equal(L.details({ ...up, revision: { client: '<b>', server: rev + 'x' } })[1][1], 'unknown');
  assert.equal(L.reasonText('pin-mismatch', down), "server's identity changed; not connecting");
  assert.equal(L.reasonText('__proto__', down), '__proto__');
});

test('reachText: a failed read names the server; nothing without an outcome', () => {
  assert.equal(L.reachText('not-sent', down), "Can't reach server.");
  assert.equal(L.reachText('unknown', up), "Can't reach server.");
  assert.equal(L.reachText('', down), null);
  assert.equal(L.reachText('not-sent', null), "Can't reach the server.");
});

test('workerLine: only with a registration found, in both modes', () => {
  assert.equal(L.workerLine(0), null);
  assert.equal(L.workerLine(undefined), null);
  assert.equal(L.workerLine(NaN), null);
  const w = L.workerLine(2);
  assert.equal(w.state, 'worker');
  assert.match(w.text, /Reset window data/);
});

test('accountWord: a plain name, else <account>', () => {
  assert.equal(L.accountWord('work'), 'work');
  for (const bad of ['$(id)', '`id`', 'a b', '', null, undefined, 'x'.repeat(33), '__proto__x;', '<b>']) assert.equal(L.accountWord(bad), '<account>', String(bad));
});

test('reasonText: an Object.prototype name is not a known reason', () => {
  for (const r of ['__proto__', 'constructor', 'toString']) assert.equal(L.reasonText(r, down), r);
});

test('the version nudge: muted tail on the line, a details row, nothing on a server', () => {
  const nudged = (state) => ({ ...up, update: { state, client: rev, server: 'unknown' } });
  assert.equal(L.nudge(nudged('client-older')), 'Update available');
  assert.equal(L.nudge(nudged('server-older')), 'Update available');
  assert.equal(L.nudge(nudged('different')), 'Update available');
  assert.equal(L.nudge(up), '');
  assert.equal(L.nudge({ ...up, update: null }), '');
  assert.equal(L.nudge(null), '');
  for (const bad of ['newer', '__proto__', 'toString', '', 3]) assert.equal(L.nudge(nudged(bad)), '', String(bad));
  assert.equal(L.nudge({ ...up, update: 'client-older' }), '');
  assert.deepEqual(L.details(nudged('server-older')).at(-1), ['update', 'server runs an older pneu · Update server (bar menu)']);
  assert.deepEqual(L.details(nudged('client-older')).at(-1), ['update', 'this machine runs an older pneu · Update pneu (bar menu)']);
  assert.equal(L.details(up).length, 3);
  // Down keeps the nudge: the line shows the link first, the tail after.
  assert.equal(L.nudge({ ...down, update: { state: 'different' } }), 'Update available');
});

test('reauthHelp: a client shows the command to run here and the bar menu; a server keeps Reconnect', () => {
  const h = L.reauthHelp('work', up);
  assert.equal(h.lead + h.command + h.tail,
    'Gmail access expired or was revoked. Run pneu account auth work in a terminal on this machine (it runs on server over SSH and opens Google here), or Fix with agent in the bar menu.');
  assert.equal(h.command, 'pneu account auth work');
  // Down too: the command waits for the link itself.
  assert.equal(L.reauthHelp('work', down).command, 'pneu account auth work');
  // A name outside the plain shape never reaches a suggested command.
  assert.equal(L.reauthHelp('$(id)', up).command, 'pneu account auth <account>');
  assert.equal(L.reauthHelp('a b; rm -rf ~', up).command, 'pneu account auth <account>');
  // A server: no link, so the app's own Reconnect.
  assert.equal(L.reauthHelp('work', null), null);
  assert.equal(L.reauthHelp('work', { state: '__proto__' }), null);
});

test('reauthRefused: only the 409 reauth-on-server', () => {
  assert.equal(L.reauthRefused({ status: 409, message: 'reauth-on-server' }), true);
  assert.equal(L.reauthRefused({ status: 409, message: 'gmi: a re-auth is already waiting' }), false);
  assert.equal(L.reauthRefused({ status: 500, message: 'reauth-on-server' }), false);
  assert.equal(L.reauthRefused(null), false);
});
