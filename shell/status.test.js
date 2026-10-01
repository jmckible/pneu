// node --test shell/status.test.js — the bar widget's decisions
// (shell/status.js): status.json versions 1 and 2, staleness by the
// daemon's clock and by the server's, the link's states, the menu, and
// that nothing from the file reaches a command.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const S = require('./status.js');

const now = Date.parse('2026-09-30T12:00:00Z');
const iso = (ms) => new Date(ms).toISOString().replace('.000Z', 'Z');
const ago = (min) => iso(now - min * 60000);
const fmt = { clock: (ms) => new Date(ms).toISOString().slice(11, 16), day: (ms) => new Date(ms).toISOString().slice(0, 10) };

const v1 = (over) => Object.assign({ version: 1, updated: ago(1), running: true, unread: 0, senders: [], accounts: [] }, over);
const v2 = (server, over) => Object.assign(v1({ version: 2 }), {
  server: Object.assign({ name: 'dell', link: 'up', linkSince: ago(30), statusAt: ago(1), reason: null, update: null }, server),
}, over);
const m = (doc, missing) => S.model(S.parse(JSON.stringify(doc)), !!missing, now);
const ids = (mm, pending) => S.menu(mm, pending).map((i) => i.id);

// The widget's tooltip as it was before version 2 (BarWidget.qml at
// 475fdee), for the version 1 parity check.
function legacy(status, missing) {
  const unreadable = !(status !== null && typeof status === 'object' && status.version === 1);
  if (missing) return 'pneu: no status yet. Is the server installed and running?\nsystemctl --user status pneu';
  if (unreadable) return 'pneu: status.json unreadable (a newer or older pneu?)';
  const accounts = Array.isArray(status.accounts) ? status.accounts : [];
  const unread = parseInt(status.unread) || 0;
  const senders = Array.isArray(status.senders) ? status.senders.map(String) : [];
  const updatedAt = Date.parse(String(status.updated || ''));
  const stale = !isFinite(updatedAt) || now - updatedAt > 20 * 60 * 1000;
  const stopped = status.running !== true;
  const sick = accounts.filter(S.accountSick);
  const pulling = accounts.filter((a) => a && (a.state === 'pulling' || a.state === 'needs-pull'));
  const clock = (ms) => (isFinite(ms) ? fmt.clock(ms) : 'never');
  const lines = [];
  if (stopped) lines.push('pneu server stopped (last update ' + clock(updatedAt) + ')');
  else if (stale) lines.push('pneu server not responding (last update ' + clock(updatedAt) + ')');
  if (unread === 0) lines.push('No unread mail');
  else {
    const names = senders.slice(0, Math.min(unread, 3));
    const extra = unread - names.length;
    lines.push(names.length === 0 ? unread + ' unread' : unread + ' unread · ' + names.join(', ') + (extra > 0 ? ' +' + extra : ''));
  }
  for (const a of pulling) lines.push(S.pullLine(a, fmt));
  for (const a of sick) lines.push(S.accountLine(a));
  return lines.join('\n');
}

test('version 1 reads exactly as before', () => {
  const docs = [
    v1(),
    v1({ unread: 5, senders: ['Ann', 'Bo', 'Cy', 'Di'] }),
    v1({ unread: 2, senders: [] }),
    v1({ running: false }),
    v1({ updated: ago(25) }),
    v1({ updated: 'nonsense' }),
    v1({ accounts: [{ name: 'work', state: 'ready', failures: 3, error: 'gmi: timeout' }, { name: 'home', state: 'reauth' }] }),
    v1({ accounts: [{ name: 'new', state: 'pulling', progress: { phase: 'content', done: 1200, total: 4000, percent: 30, frontier: '2025-01-02T00:00:00Z' } }] }),
    v1({ accounts: [{ name: 'old', pulled: false }] }),
  ];
  for (const d of docs) {
    assert.equal(S.tooltip(m(d), fmt), legacy(JSON.parse(JSON.stringify(d)), false), JSON.stringify(d));
  }
  assert.equal(S.tooltip(m(null, true), fmt), legacy(null, true));
  assert.equal(S.tooltip(S.model(S.parse('{'), false, now), fmt), legacy(null, false));
  // Warnings as before: stopped, stale, failing; not a first pull.
  assert.equal(m(v1()).warning, false);
  assert.equal(m(v1({ running: false })).warning, true);
  assert.equal(m(v1({ updated: ago(21) })).warning, true);
  assert.equal(m(v1({ accounts: [{ name: 'w', state: 'ready', failures: 1 }] })).warning, true);
  assert.equal(m(v1({ accounts: [{ name: 'w', state: 'pulling' }] })).warning, false);
  assert.equal(S.model(null, true, now).warning, true);
});

test('parse: versions 1 and 2 only, and 2 only with its server block', () => {
  assert.ok(S.parse(JSON.stringify(v1())));
  assert.ok(S.parse(JSON.stringify(v2())));
  for (const bad of [
    '', 'null', '[]', '{"version":3}', '{"version":"1"}',
    JSON.stringify(v1({ version: 2 })),
    JSON.stringify(v2({ link: 'sideways' })),
    JSON.stringify(v2({ link: '__proto__' })),
    JSON.stringify(v2({ link: 'constructor' })),
    JSON.stringify(v2({ link: 'toString' })),
    JSON.stringify(v2({ link: 'hasOwnProperty' })),
    JSON.stringify(v2({ link: ['up'] })),
    JSON.stringify(v2({ name: 7 })),
    JSON.stringify(Object.assign(v2(), { server: [] })),
  ]) assert.equal(S.parse(bad), null, bad);
});

test('version 2: counts go stale by statusAt or the link, not by updated', () => {
  const fresh = m(v2());
  assert.equal(fresh.warning, false);
  assert.equal(fresh.countsStale, false);
  // updated keeps ticking (this daemon is fine), but nothing from the
  // server for 25 minutes: stale counts, a warning, and Fix with agent.
  const quiet = m(v2({ statusAt: ago(25) }));
  assert.equal(quiet.stale, false);
  assert.equal(quiet.countsStale, true);
  assert.equal(quiet.warning, true);
  assert.match(S.tooltip(quiet, fmt), /No word from dell since 11:35/);
  assert.match(S.tooltip(quiet, fmt), /\(as of 11:35\)/);
  assert.deepEqual(ids(quiet), ['open', 'agent', 'reset']);
  // No status ever.
  assert.equal(m(v2({ statusAt: null })).countsStale, true);
  // This daemon stopped: as in version 1, but it's this machine's pneu.
  const stopped = m(v2({}, { running: false }));
  assert.equal(stopped.warning, true);
  assert.match(S.tooltip(stopped, fmt), /^pneu stopped \(last update/);
  assert.ok(ids(stopped).includes('agent'));
});

test('version 2: link down is a warning naming the server and the reason', () => {
  const down = m(v2({ link: 'down', reason: 'node-offline', linkSince: ago(10) }));
  assert.equal(down.warning, true);
  assert.equal(down.linkDown, true);
  const tip = S.tooltip(down, fmt);
  assert.match(tip, /^Can't reach dell since 11:50 · dell is offline$/m);
  assert.match(tip, /Right-click: Fix with agent/);
  assert.deepEqual(ids(down), ['open', 'agent', 'reset']);
  // A reason this build doesn't know is just "can't reach".
  assert.match(S.tooltip(m(v2({ link: 'down', reason: 'brand-new', linkSince: ago(1) })), fmt), /^Can't reach dell since 11:59$/m);
  // A reason named like an Object.prototype member is just "can't reach".
  for (const r of ['__proto__', 'constructor', 'toString']) {
    assert.match(S.tooltip(m(v2({ link: 'down', reason: r, linkSince: ago(1) })), fmt), /^Can't reach dell since 11:59$/m, r);
  }
  // A protocol mismatch.
  assert.ok(ids(m(v2({ link: 'down', reason: 'protocol' }))).includes('agent'));
});

test('version 2: starting is neutral at first, then unreachable', () => {
  const starting = m(v2({ link: 'starting', reason: 'starting', linkSince: iso(now - 10000), statusAt: null }));
  assert.equal(starting.connecting, true);
  assert.equal(starting.warning, false);
  assert.match(S.tooltip(starting, fmt), /^Connecting to dell…$/m);
  assert.doesNotMatch(S.tooltip(starting, fmt), /as of/);
  assert.deepEqual(ids(starting), ['open', 'reset']);
  const long = m(v2({ link: 'starting', reason: 'starting', linkSince: iso(now - S.CONNECT_GRACE - 1000) }));
  assert.equal(long.connecting, false);
  assert.equal(long.warning, true);
  assert.ok(ids(long).includes('agent'));
});

test('menu: Fix with agent for failing accounts in both versions; Reopen only after a reset', () => {
  const sick = { name: 'work', state: 'ready', failures: 2, error: 'x' };
  assert.deepEqual(ids(m(v1())), ['open', 'reset']);
  assert.deepEqual(ids(m(v1({ accounts: [sick] }))), ['open', 'agent', 'reset']);
  assert.deepEqual(ids(m(v2({}, { accounts: [sick] }))), ['open', 'agent', 'reset']);
  assert.deepEqual(ids(m(v1()), true), ['open', 'reopen', 'reset']);
  assert.match(S.accountLine(sick, m(v2())), /^work: 2 failed syncs · x$/);
  assert.match(S.accountLine({ name: 'w', state: 'unauthorized' }, m(v2())), /pneu account auth w on dell\)$/);
});

// Every string a hostile server can put in a client's status.json, and
// every string in a version 1 file: shown as text (cleaned), never in a
// command. Commands are the fixed argv, whatever the file says.
test('nothing from the file reaches a command', () => {
  const evil = '"; rm -rf ~; echo "<b>$(id)</b>\u202e\n`reboot`';
  const doc = v2({ name: evil, reason: evil, link: 'down', update: { state: 'server-older', client: evil, server: evil } }, {
    senders: [evil, '<img src=x onerror=alert(1)>'],
    accounts: [{ name: evil, state: 'reauth', error: evil, failures: 9 }],
  });
  const mm = m(doc);
  const strings = [];
  (function walk(v) {
    if (typeof v === 'string') strings.push(v);
    else if (v && typeof v === 'object') Object.values(v).forEach(walk);
  })(doc);
  const fixed = {};
  for (const [id, it] of Object.entries(S.ITEMS)) fixed[id] = JSON.stringify(it.argv);
  for (const pending of [false, true]) {
    for (const item of S.menu(mm, pending)) {
      const argv = S.argv(item.id);
      assert.equal(JSON.stringify(argv), fixed[item.id]);
      // Update <server> is the one label with a name: the server's,
      // cleaned (no newline, no bidi), drawn as plain text.
      if (item.id === 'updateServer') assert.equal(item.label, 'Update ' + S.clean(evil, 64));
      else assert.equal(item.label, S.ITEMS[item.id].label);
      assert.ok(!/[\n\u202e]/.test(item.label));
      for (const s of strings) {
        if (s.length < 3) continue;
        assert.ok(!argv.some((a) => a.includes(s)), `${item.id}: ${s}`);
        if (item.id !== 'updateServer') assert.ok(!item.label.includes(s));
      }
    }
  }
  // argv hands out copies: changing one doesn't change the table.
  const a = S.argv('agent');
  a.push('--evil');
  assert.deepEqual(S.argv('agent'), ['pneu', 'agent']);
  assert.equal(S.argv('nope'), null);
  for (const k of ['__proto__', 'constructor', 'toString', 'hasOwnProperty', 'valueOf']) assert.equal(S.argv(k), null, k);
  // Shown as text, cleaned: markup kept literally, controls and bidi gone.
  const tip = S.tooltip(mm, fmt);
  assert.ok(tip.includes('<img src=x onerror=alert(1)>') || tip.includes('<b>$(id)</b>'));
  assert.ok(!/[\u202e]/.test(tip));
  // The newline in the name is gone: it can't start a line of its own.
  assert.ok(tip.includes("Can't reach \"; rm -rf ~; echo \"<b>$(id)</b>`reboot` since"), tip);
  assert.ok(!tip.split('\n').some((l) => l.startsWith('`reboot`')));
});

// The version nudge (server.update): Update pneu when this machine is
// behind or the builds just differ, Update <server> when the server is
// behind; nothing for anything else, and nothing in version 1.
test('menu: the update items by server.update', () => {
  const up = (state, extra) => m(v2({ update: Object.assign({ state, client: 'a'.repeat(40), server: 'b'.repeat(40) }, extra) }));
  assert.deepEqual(ids(up('client-older')), ['open', 'update', 'reset']);
  assert.deepEqual(ids(up('different')), ['open', 'update', 'reset']);
  assert.deepEqual(ids(up('server-older')), ['open', 'updateServer', 'reset']);
  assert.equal(S.menu(up('server-older'), false)[1].label, 'Update dell');
  for (const bad of ['newer', '__proto__', 'toString', 'constructor', 'client-older\n', '', 1, null]) {
    assert.deepEqual(ids(up(bad)), ['open', 'reset'], String(bad));
  }
  for (const bad of [null, 'client-older', ['client-older'], { state: { toString: 1 } }]) {
    assert.deepEqual(ids(m(v2({ update: bad }))), ['open', 'reset'], JSON.stringify(bad));
  }
  // An update never makes a warning, and a link down keeps Fix with agent first.
  assert.equal(up('client-older').warning, false);
  assert.deepEqual(ids(m(v2({ link: 'down', reason: 'refused', update: { state: 'server-older' } }))), ['open', 'agent', 'updateServer', 'reset']);
  // Version 1 has no server block to nudge from.
  assert.deepEqual(ids(m(Object.assign(v1(), { server: { update: { state: 'client-older' } } }))), ['open', 'reset']);
  // The commands are the fixed ones: a floating terminal running pneu
  // update, and the agent's version situation.
  assert.deepEqual(S.argv('update'), ['omarchy-launch-floating-terminal-with-presentation', 'pneu', 'update']);
  assert.deepEqual(S.argv('updateServer'), ['pneu', 'agent', '-update']);
  // The tooltip says so; the revisions themselves never appear.
  assert.match(S.tooltip(up('client-older'), fmt), /Update available: dell runs a newer pneu · right-click: Update pneu$/);
  assert.match(S.tooltip(up('server-older'), fmt), /dell runs an older pneu · right-click: Update dell$/);
  assert.ok(!S.tooltip(up('different'), fmt).includes('a'.repeat(40)));
});

test('word: a name in a suggested command, or <account>', () => {
  assert.equal(S.word('work'), 'work');
  for (const bad of ['$(id)', '`id`', 'a b', '', null, 'x'.repeat(33), '-x', '<b>']) assert.equal(S.word(bad), '<account>', String(bad));
  assert.match(S.accountLine({ name: '$(id)', state: 'unauthorized' }, m(v2())), /pneu account auth <account> on dell\)$/);
  assert.match(S.accountLine({ name: '$(id)', state: 'unconfigured' }, m(v1())), /pneu account add <account> <address>\)$/);
});

test('clean: controls, bidi and separators dropped; capped', () => {
  assert.equal(S.clean('a\u0000b\u202ec\u2028d\te'), 'abcde');
  assert.equal(S.clean(null), '');
  assert.equal(S.clean('x'.repeat(200)).length, 129);
  assert.equal(S.clean('<b>bold</b>'), '<b>bold</b>');
});

// The widget draws every string from the file through Text.PlainText:
// its own Text items say so, and the menu's Buttons and the bar's tooltip
// are the shell's, which render PlainText (qs.Ui Button, Bar.qml).
test('BarWidget.qml: every Text is PlainText, commands come from status.js', () => {
  const qml = fs.readFileSync(path.join(__dirname, 'BarWidget.qml'), 'utf8');
  const texts = qml.split(/\bText\s*\{/).slice(1);
  assert.ok(texts.length >= 3);
  for (const t of texts) assert.match(t.slice(0, 400), /textFormat:\s*Text\.PlainText/);
  assert.doesNotMatch(qml, /RichText|StyledText|MarkdownText/);
  // Commands: only Status.argv(<id>) into execArgv, never bar.run with
  // anything but the user's own command setting.
  for (const line of qml.split('\n')) {
    if (/execArgv\(/.test(line)) assert.match(line, /execArgv\(Status\.argv\(id\)\)/);
    if (/bar\.run\(/.test(line)) assert.match(line, /bar\.run\(command !== "" \? command : "pneu open"\)/);
  }
});
