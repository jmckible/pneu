// node --test web/push.test.js — the sync details' instant mail lines
// and polling note (web/static/push.js; docs/push.md D7).
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const P = require('./static/push.js');
const L = require('./static/link.js');

const now = Date.parse('2026-10-03T12:00:00Z');
const ago = (ms) => (ms < 60000 ? 'just now' : Math.floor(ms / 60000) + 'm ago');
const server = { word: 'personal', server: null, now, ago };
const client = { ...server, server: 'dell' };
// poll's per has no prototype; compare it as a plain object.
const poll = (a, f) => { const r = P.poll(a, f); return { foot: r.foot, per: { ...r.per } }; };

test('line: each state in words; absent or anything unknown is off', () => {
  assert.deepEqual(P.line({ state: 'delivering', lastDelivery: '2026-10-03T11:58:00Z' }, server), { text: 'Instant: delivering · last message 2m ago', fix: null });
  assert.equal(P.line({ state: 'delivering', lastDelivery: '2026-10-03T12:00:30Z' }, server).text, 'Instant: delivering · last message just now');
  assert.equal(P.line({ state: 'delivering' }, server).text, 'Instant: delivering');
  assert.equal(P.line({ state: 'delivering', lastDelivery: 'yesterday' }, server).text, 'Instant: delivering');
  assert.deepEqual(P.line({ state: 'quiet' }, server), { text: 'Instant: quiet', fix: null });
  assert.deepEqual(P.line({ state: 'starting' }, server), { text: 'Instant: starting', fix: null });
  for (const p of [undefined, null, {}, 'delivering', { state: 'off' }, { state: 'constructor' }, { state: 'toString' },
    { state: '__proto__' }, { state: '<b>x</b>' }, { state: ['quiet'] }]) {
    assert.deepEqual(P.line(p, server), { text: 'Instant: off', fix: null }, JSON.stringify(p));
  }
});

test('line: failing and reauth name the reason in words and the fix', () => {
  assert.deepEqual(P.line({ state: 'reauth', reason: 'mailbox-reauth' }, server), {
    text: "Instant: failing — this mailbox's grant for instant mail expired or was revoked",
    fix: { lead: 'Run ', command: 'pneu account push personal', tail: '.' },
  });
  assert.deepEqual(P.line({ state: 'reauth', reason: 'owner-reauth' }, server).fix, { lead: 'Run ', command: 'pneu push init --reconsent', tail: '.' });
  for (const r of ['api-disabled', 'permission', 'org-policy']) {
    assert.deepEqual(P.line({ state: 'failing', reason: r }, server).fix, { lead: '', command: 'pneu account push personal', tail: ' prints the fix.' }, r);
  }
  for (const r of ['watch-expired', 'unknown']) {
    assert.deepEqual(P.line({ state: 'failing', reason: r }, server).fix, { lead: 'Run ', command: 'pneu account push personal', tail: '.' }, r);
  }
  assert.deepEqual(P.line({ state: 'failing', reason: 'network' }, server), { text: "Instant: failing — can't reach Google; it keeps trying", fix: null });
  // A reason outside the state's list (or none): unknown's words, never the value.
  for (const p of [{ state: 'failing' }, { state: 'failing', reason: 'owner-reauth' }, { state: 'reauth', reason: 'network' },
    { state: 'failing', reason: '<img src=x>' }, { state: 'failing', reason: 'hasOwnProperty' }]) {
    const l = P.line(p, server);
    assert.equal(l.text, 'Instant: failing — something went wrong', JSON.stringify(p));
    assert.equal(l.fix.command, 'pneu account push personal');
  }
  // The account as a command shows it (app.js passes link.js accountWord).
  assert.equal(P.line({ state: 'reauth', reason: 'mailbox-reauth' }, { ...server, word: L.accountWord('a b') }).fix.command, 'pneu account push <account>');
});

test('line: on a client the command runs in a terminal here, on the server over SSH', () => {
  assert.deepEqual(P.line({ state: 'reauth', reason: 'owner-reauth' }, client).fix, {
    lead: 'Run ', command: 'pneu push init --reconsent',
    tail: ' in a terminal on this machine (it runs on dell over SSH and opens Google here if it needs to).',
  });
  assert.equal(P.line({ state: 'failing', reason: 'permission' }, client).fix.tail,
    ' prints the fix; run it in a terminal on this machine (it runs on dell over SSH and opens Google here if it needs to).');
  assert.equal(L.remoteName({ state: 'up', server: 'dell' }), 'dell');
  assert.equal(L.remoteName({ state: 'down', server: '' }), 'the server');
  assert.equal(L.remoteName(null), null);
  assert.equal(L.remoteName({ state: 'weird', server: 'dell' }), null);
});

test('poll: the footer from the ready accounts, a slower one noted', () => {
  assert.deepEqual(poll([{ name: 'a', state: 'ready', pollEvery: 30 }, { name: 'b', state: 'ready', pollEvery: 30 }], 120),
    { foot: 'Checks every 30s', per: {} });
  assert.deepEqual(poll([{ name: 'a', state: 'ready', pollEvery: 30 }, { name: 'b', state: 'ready', pollEvery: 240 }], 30),
    { foot: 'Checks every 30s', per: { b: 'checks every 4m while failing' } });
  // One waiting on setup looks again sooner; it doesn't set the footer.
  assert.deepEqual(poll([{ name: 'a', state: 'unconfigured', pollEvery: 5 }, { name: 'b', state: 'ready', pollEvery: 30 }], 30),
    { foot: 'Checks every 30s', per: {} });
  // No pollEvery (an older server), or one out of shape: the page's period.
  assert.deepEqual(poll([{ name: 'a', state: 'ready' }], 30), { foot: 'Checks every 30s', per: {} });
  for (const bad of [0, -1, 1.5, '30', 86401, NaN, Infinity, null]) {
    assert.deepEqual(poll([{ name: 'a', state: 'ready', pollEvery: bad }], 60), { foot: 'Checks every 1m', per: {} }, String(bad));
  }
  assert.deepEqual(poll([], 120), { foot: 'Checks every 2m', per: {} });
  // Valid account names that are Object.prototype's: nothing inherited.
  const names = P.poll(['constructor', 'toString', '__proto__', 'hasOwnProperty'].map((name) => ({ name, state: 'ready', pollEvery: 30 })), 30);
  for (const n of ['constructor', 'toString', '__proto__', 'hasOwnProperty']) assert.equal(names.per[n], undefined, n);
  const slow = P.poll([{ name: 'a', state: 'ready', pollEvery: 30 }, { name: 'constructor', state: 'ready', pollEvery: 120 }], 30);
  assert.equal(slow.per.constructor, 'checks every 2m while failing');
  assert.equal(P.every(900), '15m');
  assert.equal(P.every(7200), '2h');
});
