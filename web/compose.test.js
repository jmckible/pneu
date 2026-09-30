// node --test web/compose.test.js — the pure parts of compose (web/static/compose.js).
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const C = require('./static/compose.js');

// A hand-built node tree with the interface htmlToText walks.
function h(name, attrs, ...kids) {
  if (typeof attrs === 'string' || Array.isArray(attrs) || (attrs && attrs.nodeType)) { kids.unshift(attrs); attrs = {}; }
  attrs = attrs || {};
  const childNodes = kids.flat().map((k) => (typeof k === 'string' ? t(k) : k));
  return {
    nodeType: 1, nodeName: name.toUpperCase(), childNodes,
    getAttribute: (a) => (Object.prototype.hasOwnProperty.call(attrs, a) ? attrs[a] : null),
    get textContent() { return childNodes.map((c) => c.textContent).join(''); },
  };
}
function t(s) { return { nodeType: 3, nodeName: '#text', childNodes: [], textContent: s }; }

test('htmlToText: paragraphs, divs, br, whitespace collapse', () => {
  const doc = h('body',
    h('p', '  Hello\n   there,  '),
    h('div', 'line one', h('br'), '  line two'),
    h('div', h('div', 'nested')),
    h('p', 'a', h('br'), h('br'), h('br'), h('br'), 'b'));
  assert.equal(C.htmlToText(doc), 'Hello there,\n\nline one\nline two\nnested\n\na\n\nb');
});

test('htmlToText: head, style, script, hidden skipped; invisible padding dropped', () => {
  const doc = h('html', h('head', h('title', 'T'), h('style', 'p{}')),
    h('body', h('div', { style: 'color:red; DISPLAY: none' }, 'preheader'),
      h('div', 'vis\u200c\u034f\u00a0ible'), h('p', 'body'), h('script', 'x()')));
  assert.equal(C.htmlToText(doc), 'vis ible\n\nbody');
});

test('htmlToText: lists get "- ", nested lists indent', () => {
  const doc = h('body', h('p', 'Items:'),
    h('ul', h('li', 'one'), h('li', h('p', 'two'), h('ul', h('li', 'two-a'))), h('li', 'three')),
    'after');
  assert.equal(C.htmlToText(doc), 'Items:\n\n- one\n- two\n\n  - two-a\n- three\n\nafter');
});

test('htmlToText: links show href only when it differs from the text', () => {
  const doc = h('body',
    h('p', 'See ', h('a', { href: 'https://example.com/x' }, 'the docs'), '.'),
    h('p', h('a', { href: 'https://example.com/y' }, 'https://example.com/y')),
    h('p', h('a', { href: 'mailto:a@example.com' }, 'a@example.com')),
    h('p', h('a', { href: '#top' }, 'top')),
    h('p', h('a', { href: 'https://track.example/z' }, h('img', { src: 'x' }))));
  assert.equal(C.htmlToText(doc),
    'See the docs <https://example.com/x>.\n\nhttps://example.com/y\n\na@example.com\n\ntop');
});

test('htmlToText: blockquote lines get "> ", nested twice', () => {
  const doc = h('body', h('p', 'Reply'),
    h('blockquote', h('p', 'quoted'), h('p', 'more'), h('blockquote', 'deeper')), 'end');
  assert.equal(C.htmlToText(doc), 'Reply\n\n> quoted\n>\n> more\n>\n> > deeper\n\nend');
});

test('htmlToText: pre keeps its whitespace; tables are space-separated cells', () => {
  const doc = h('body', h('pre', '  a\n    b'),
    h('table', h('tbody', h('tr', h('td', 'Total'), h('td', '$5')), h('tr', h('td', 'Tax'), h('td', '$1')))));
  assert.equal(C.htmlToText(doc), '  a\n    b\n\nTotal $5\nTax $1');
});

test('htmlToText: img alt text, empty input', () => {
  assert.equal(C.htmlToText(h('body', h('img', { alt: 'Logo' }), ' Hi')), 'Logo Hi');
  assert.equal(C.htmlToText(h('body')), '');
  assert.equal(C.htmlToText(null), '');
});

test('quote: "> " per line, ">" on blank, newline-terminated like the server', () => {
  assert.equal(C.quote('a\n\nb'), '> a\n>\n> b\n');
  assert.equal(C.quote('> x'), '> > x\n');
  assert.equal(C.quote('\n\n  \n'), '');
  assert.equal(C.quote('a\r\n\r\n\r\nb\n\n'), '> a\n>\n> b\n');
});

test('tokenAt: the recipient around the caret', () => {
  assert.deepEqual(C.tokenAt('jo', 2), { start: 0, end: 2, query: 'jo' });
  const v = 'a@x.example, bo, c@y';
  assert.deepEqual(C.tokenAt(v, 15), { start: 13, end: 15, query: 'bo' });
  assert.deepEqual(C.tokenAt(v, 14), { start: 13, end: 15, query: 'b' });
  assert.deepEqual(C.tokenAt(v, v.length), { start: 17, end: v.length, query: 'c@y' });
  assert.equal(C.tokenAt('a@x, ', 5).query, '');
  assert.equal(C.tokenAt('a@x; da', 7).query, 'da');
});

test('tokenAt: commas inside a quoted name do not split', () => {
  const v = '"Doe, Jane" <j@x>, mi';
  assert.deepEqual(C.tokenAt(v, v.length), { start: 19, end: v.length, query: 'mi' });
  assert.deepEqual(C.tokenAt(v, 4), { start: 0, end: 17, query: '"Doe' });
  assert.equal(C.tokenAt('"a\\"b, c" <z>, q', 16).query, 'q');
});

test('replaceToken: swaps the token and appends ", "', () => {
  let v = 'a@x.example, bo';
  let r = C.replaceToken(v, C.tokenAt(v, v.length), 'Bob <bob@example.com>');
  assert.deepEqual(r, { value: 'a@x.example, Bob <bob@example.com>, ', caret: 36 });

  r = C.replaceToken('jo', C.tokenAt('jo', 2), 'Jo <jo@x>');
  assert.deepEqual(r, { value: 'Jo <jo@x>, ', caret: 11 });

  // In the middle: the old separator isn't doubled, the rest is kept.
  v = 'bo, c@y';
  r = C.replaceToken(v, C.tokenAt(v, 2), 'Bob <b@x>');
  assert.equal(r.value, 'Bob <b@x>, c@y');
  assert.equal(r.caret, 11);

  // No space after the comma before the token: one is added.
  v = 'a@x,bo';
  r = C.replaceToken(v, C.tokenAt(v, 6), 'Bob <b@x>');
  assert.equal(r.value, 'a@x, Bob <b@x>, ');
});

test('drafts: parse, compare, prune to the newest', () => {
  const d = { to: 'a', cc: '', bcc: '', subject: 's', body: 'b', at: 5 };
  assert.deepEqual(C.parseDraft(JSON.stringify(d)), { ...d, sent: false });
  assert.equal(C.parseDraft('{"to":"a"}'), null);
  assert.equal(C.parseDraft('nope'), null);
  assert.equal(C.parseDraft('null'), null);
  assert.equal(C.parseDraft(JSON.stringify({ ...d, at: 'x' })).at, 0);

  assert.equal(C.sameFields(d, { ...d, at: 9 }), true);
  assert.equal(C.sameFields(d, { ...d, body: 'c' }), false);

  const entries = Array.from({ length: 25 }, (_, i) => ({ key: 'k' + i, at: (i * 7) % 25 }));
  const gone = C.prune(entries, 20);
  assert.equal(gone.length, 5);
  const keptAts = entries.filter((e) => !gone.includes(e.key)).map((e) => e.at);
  assert.ok(Math.min(...keptAts) > Math.max(...entries.filter((e) => gone.includes(e.key)).map((e) => e.at)));
  assert.deepEqual(C.prune(entries.slice(0, 3)), []);
  assert.equal(C.DRAFT_CAP, 20);
});

test('settle: only the send\'s own redirect drops the draft', () => {
  const id = '<0123456789abcdef0123456789abcdef@pneu.dell>';
  const m = C.parseMarker(JSON.stringify({ key: 'pneu:draft:reply:personal:a@b', id, at: 1 }));
  assert.ok(m);
  const keep = { drop: null, clear: false, id: null };

  assert.deepEqual(C.settle(null, { sent: true }), keep);
  // Redirected by the send (#sent): the message went out.
  assert.deepEqual(C.settle(m, { sent: true }), { drop: m.key, clear: true, id: null });
  // Anywhere else (Back, u, a list): the POST may still be in flight or dead.
  assert.deepEqual(C.settle(m, { compose: false }), keep);
  assert.deepEqual(C.settle(m, { compose: true, key: 'pneu:draft:compose:personal' }), keep);
  // The failed send's own re-render (error banner): keep the draft, drop the marker.
  assert.deepEqual(C.settle(m, { compose: true, key: m.key, error: true }), { drop: null, clear: true, id: null });
  // Back on the same draft with no answer: keep both, and the marker's id
  // for a draft that lost its own.
  assert.deepEqual(C.settle(m, { compose: true, key: m.key, error: false }), { drop: null, clear: false, id });
  // A marker id that isn't a pneu id is none.
  assert.equal(C.parseMarker(JSON.stringify({ key: m.key, id: '<x@evil>' })).id, '');
});

test('persistForSend: no send unless the id is in local storage', () => {
  const id = '<0123456789abcdef0123456789abcdef@pneu.dell>';
  const key = 'pneu:draft:compose:personal';
  const draft = { to: 'a', cc: '', bcc: '', subject: 's', body: 'b', at: 5, id, idAt: 5, sent: true };
  const mem = () => {
    const m = new Map();
    return { setItem: (k, v) => m.set(k, String(v)), getItem: (k) => (m.has(k) ? m.get(k) : null), m };
  };
  const throwing = { setItem() { throw new Error('QuotaExceededError'); }, getItem() { throw new Error('SecurityError'); } };
  const dropping = { setItem() {}, getItem() { return null; } }; // accepts, keeps nothing

  let l = mem(), s = mem();
  assert.equal(C.persistForSend(l, s, key, draft, 9), true);
  assert.equal(C.parseDraft(l.getItem(key)).id, id);
  assert.equal(C.parseMarker(s.getItem(C.SENDING_KEY)).id, id);

  s = mem();
  assert.equal(C.persistForSend(throwing, s, key, draft, 9), false);
  assert.equal(s.getItem(C.SENDING_KEY), null); // no marker for a send that won't go
  assert.equal(C.persistForSend(dropping, mem(), key, draft, 9), false);
  // Session storage failing only loses the second copy.
  l = mem();
  assert.equal(C.persistForSend(l, throwing, key, draft, 9), true);
});

test('submitDraft: pruning never costs the submitted draft', () => {
  const mem = () => {
    const m = new Map();
    return {
      setItem: (k, v) => m.set(k, String(v)), getItem: (k) => (m.has(k) ? m.get(k) : null),
      removeItem: (k) => m.delete(k), key: (i) => [...m.keys()][i] ?? null, get length() { return m.size; }, m,
    };
  };
  const id = '<0123456789abcdef0123456789abcdef@pneu.dell>';
  const base = { to: 'a', cc: '', bcc: '', subject: 's', body: 'b' };
  const key = 'pneu:draft:compose:personal';
  const now = 1000;
  // Twenty drafts stamped ahead of a clock that was since corrected, and
  // one older submitted draft with an unresolved send.
  const l = mem(), s = mem();
  for (let i = 0; i < 20; i++) l.setItem('pneu:draft:reply:p:' + i, JSON.stringify({ ...base, at: now + 1e9 + i }));
  const sentKey = 'pneu:draft:reply:p:sent';
  l.setItem(sentKey, JSON.stringify({ ...base, at: 1, id, idAt: 1, sent: true }));
  const draft = { ...base, at: now, id, idAt: now, sent: true };
  assert.equal(C.submitDraft(l, s, key, draft, now), true);
  assert.equal(C.parseDraft(l.getItem(key)).id, id);
  assert.ok(l.getItem(sentKey), 'a submitted draft was evicted');
  assert.equal([...l.m.keys()].filter((k) => k.startsWith(C.DRAFT_PREFIX) && !C.parseDraft(l.getItem(k)).sent).length, C.DRAFT_CAP - 1);
  // Later saves trim the same way: the active and submitted drafts stay.
  C.capDrafts(l, key);
  assert.ok(l.getItem(key) && l.getItem(sentKey));
});

test('draft ids: kept with the draft, dropped past the server\'s record', () => {
  const id = '<0123456789abcdef0123456789abcdef@pneu.dell>';
  const base = { to: 'a', cc: '', bcc: '', subject: 's', body: 'b', at: 5 };
  const d = C.parseDraft(JSON.stringify({ ...base, id, idAt: 1000, sent: true }));
  assert.equal(d.id, id);
  assert.equal(d.sent, true);
  assert.equal(C.draftID(d, 1000), id);
  assert.equal(C.draftID(d, 1000 + C.ID_KEEP - 1), id);
  // Submitted: the id stays whatever the clock does; only a discard drops it.
  assert.equal(C.draftID(d, 1000 + 10 * C.ID_KEEP), id);
  assert.equal(C.draftID(d, 0), id);
  // Never submitted: an id older than the server's record is dropped, and
  // one from the future (a clock gone backwards) too.
  const unsent = { ...d, sent: false };
  assert.equal(C.draftID(unsent, 1000 + C.ID_KEEP - 1), id);
  assert.equal(C.draftID(unsent, 1000 + C.ID_KEEP), null);
  assert.equal(C.draftID(unsent, 999), null);
  assert.equal(C.ID_KEEP, 30 * 24 * 60 * 60 * 1000);
  // Anything that isn't a pneu id is no id.
  for (const bad of ['<x@pneu.h>', '<0123456789ABCDEF0123456789ABCDEF@pneu.dell>', id + ' ', '<0123456789abcdef0123456789abcdef@evil>', 7]) {
    const p = C.parseDraft(JSON.stringify({ ...base, id: bad, idAt: 1000 }));
    assert.equal(p.id, undefined, String(bad));
    assert.equal(C.draftID(p, 1000), null);
  }
  const noAt = C.parseDraft(JSON.stringify({ ...base, id, sent: false }));
  assert.equal(C.draftID(noAt, 1000), null);
  // A draft saved before ids were kept.
  const old = C.parseDraft(JSON.stringify(base));
  assert.equal(old.sent, false);
  assert.equal(C.draftID(old, 1000), null);
  assert.equal(C.draftID(null, 1000), null);
});

test('parseMarker rejects junk and foreign keys', () => {
  assert.equal(C.parseMarker(null), null);
  assert.equal(C.parseMarker('{'), null);
  assert.equal(C.parseMarker(JSON.stringify({ key: 'pneu:undo' })), null);
  assert.equal(C.parseMarker(JSON.stringify({ key: 'pneu:draft:compose:p' })).id, '');
  assert.equal(C.SENDING_KEY, 'pneu:sending');
});
