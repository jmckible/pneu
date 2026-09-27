// node --test web/triage.test.js — the pure parts of triage (web/static/triage.js).
// Lives outside web/static so the embed doesn't serve it.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const T = require('./static/triage.js');

// Go's url.QueryEscape, as data-msgids and /tag responses carry ids.
const qesc = (s) => encodeURIComponent(s)
  .replace(/[!'()*~]/g, (c) => '%' + c.charCodeAt(0).toString(16).toUpperCase())
  .replace(/%20/g, '+');

test('body: form fields as tag.go reads them', () => {
  const ids = qesc('northwind/app/pull/4821@codehost.example') + ' ' + qesc('a+b@x.example');
  const f = new URLSearchParams(T.body('archive', { account: 'work', ids, thread: '0000000000000abc' }));
  assert.equal(f.get('action'), 'archive');
  assert.equal(f.get('account'), 'work');
  assert.equal(f.get('thread'), '0000000000000abc');
  // The form layer hands the server the escaped tokens unchanged; PathUnescape then decodes them.
  assert.equal(f.get('ids'), ids);
  assert.deepEqual(f.get('ids').split(' ').map(decodeURIComponent),
    ['northwind/app/pull/4821@codehost.example', 'a+b@x.example']);
});

test('body: ids as array or messy string, no thread when absent', () => {
  const f = new URLSearchParams(T.body('read', { account: 'p', ids: '  a%40x  b%40x ' }));
  assert.equal(f.get('ids'), 'a%40x b%40x');
  assert.equal(f.has('thread'), false);
  assert.equal(new URLSearchParams(T.body('star', { account: 'p', ids: ['a', 'b'] })).get('ids'), 'a b');
  // No ids at all: the field is left out (tag.go then reads thread= for archive/trash).
  assert.equal(T.body('archive', { account: 'p', thread: 'ab' }), 'action=archive&account=p&thread=ab');
});

test('removeArgs: ids up to the cap, the thread alone past it', () => {
  const few = Array.from({ length: T.MAX_IDS }, (_, i) => 'm' + i + '%40x').join(' ');
  assert.deepEqual(T.removeArgs('p', few, 'ab'), { account: 'p', ids: few, thread: 'ab' });
  const many = few + ' extra%40x';
  assert.deepEqual(T.removeArgs('p', many, 'ab'), { account: 'p', thread: 'ab' });
  const f = new URLSearchParams(T.body('trash', T.removeArgs('p', many, 'ab')));
  assert.equal(f.has('ids'), false);
  assert.equal(f.get('thread'), 'ab');
  // Without a thread id there is nothing to fall back to: send the ids (the server refuses).
  assert.deepEqual(T.removeArgs('p', many, ''), { account: 'p', ids: many, thread: '' });
  assert.equal(T.MAX_IDS, 500);
});

test('body: undo carries only action and id', () => {
  assert.equal(T.body('undo', { id: 'deadbeef', account: 'p', ids: 'x' }), 'action=undo&id=deadbeef');
  assert.equal(T.body('undo', {}), 'action=undo');
});

test('decodeId equates QueryEscape and PathEscape forms', () => {
  const id = 'northwind/app/pull/4821@codehost.example';
  assert.equal(T.decodeId(qesc(id)), id);
  assert.equal(T.decodeId('northwind%2Fapp%2Fpull%2F4821@codehost.example'), id); // url.PathEscape
  assert.equal(T.decodeId(qesc('a+b@x')), 'a+b@x');
  assert.equal(T.decodeId('%zz'), '%zz');
});

test('splitIds', () => {
  assert.deepEqual(T.splitIds(' a  b '), ['a', 'b']);
  assert.deepEqual(T.splitIds(''), []);
  assert.deepEqual(T.splitIds(undefined), []);
});

test('nextIndex', () => {
  assert.equal(T.nextIndex(0, 3), 0);
  assert.equal(T.nextIndex(2, 3), 2);
  assert.equal(T.nextIndex(3, 3), 2); // removed the last: previous row
  assert.equal(T.nextIndex(0, 0), -1);
});

test('undo stack push/pop with cap', () => {
  let st = [];
  for (let i = 0; i < 25; i++) st = T.push(st, { id: i.toString(16).padStart(4, '0'), action: 'archive' });
  assert.equal(st.length, T.UNDO_CAP);
  assert.equal(st[0].id, '0005');
  const before = st.slice();
  const p = T.pop(st);
  assert.equal(p.entry.id, '0018');
  assert.equal(p.stack.length, 19);
  assert.deepEqual(st, before, 'pop is pure');
  assert.deepEqual(T.pop([]), { entry: null, stack: [] });
  assert.equal(T.push(null, { id: 'a', action: 'x' }).length, 1);
});

test('parse drops junk', () => {
  assert.deepEqual(T.parse(null), []);
  assert.deepEqual(T.parse('{'), []);
  assert.deepEqual(T.parse('{"id":"a"}'), []);
  assert.deepEqual(T.parse('[{"id":"ab12","action":"trash"},{"id":"<x>","action":"trash"},null]'),
    [{ id: 'ab12', action: 'trash' }]);
});

test('removes: which views lose the row', () => {
  assert.equal(T.removes('archive', 'inbox'), true);
  assert.equal(T.removes('archive', 'starred'), false); // still flagged
  assert.equal(T.removes('archive', 'search'), false);
  for (const v of ['inbox', 'starred', 'search', 'spam', 'all']) assert.equal(T.removes('trash', v), true); // exclude_tags=trash
  assert.equal(T.removes('trash', 'trash'), false);
  for (const v of ['spam', 'trash', 'all']) assert.equal(T.removes('archive', v), false); // -inbox: still matches
  assert.equal(T.removes('star', 'inbox'), false);
});

test('retryAfter', () => {
  assert.equal(T.retryAfter('2'), 2000);
  assert.equal(T.retryAfter(null), 2000);
  assert.equal(T.retryAfter('600'), 10000);
});

test('skip: trash is a no-op in trash and refused in spam', () => {
  assert.equal(T.skip('trash', 'trash'), 'Already in trash');
  assert.ok(T.skip('trash', 'spam'));
  for (const v of ['inbox', 'starred', 'all', 'search', '', undefined]) assert.equal(T.skip('trash', v), '');
  for (const v of ['spam', 'trash']) assert.equal(T.skip('archive', v), '');
});

test('pathKind: threads, index views, everything else', () => {
  assert.equal(T.pathKind('/t/personal/0000000000000abc'), 'thread');
  assert.equal(T.pathKind('/t/work%20mail/ab'), 'thread');
  for (const p of ['/', '/starred', '/sent', '/spam', '/trash', '/all', '/search?q=from%3Ax', '/?page=2']) assert.equal(T.pathKind(p), 'list', p);
  for (const p of ['/compose', '/reply/p/x', '/t/p', '/t/p/x/y', '', undefined, '/starred/x']) assert.equal(T.pathKind(p), null, String(p));
});

test('listURL: only a same-origin index view, else the inbox', () => {
  assert.equal(T.listURL('/starred'), '/starred');
  assert.equal(T.listURL('/search?q=a%20b'), '/search?q=a%20b');
  assert.equal(T.listURL('/?page=3'), '/?page=3');
  for (const v of [null, undefined, '', 'starred', '//evil.example/', '/\\evil.example', '/t/p/ab', '/compose', 42]) {
    assert.equal(T.listURL(v), '/', String(v));
  }
});

test('historyOp: opens push, the pane moving on by itself replaces', () => {
  assert.equal(T.historyOp('/', '/t/p/a', true), 'push'); // Enter from the list: Back returns to it
  assert.equal(T.historyOp('/t/p/a', '/t/p/b', true), 'push');
  assert.equal(T.historyOp('/t/p/a', '/t/p/b', false), 'replace'); // auto-advance after e
  assert.equal(T.historyOp('/t/p/a', '/', false), 'replace'); // Esc or an emptied list
  assert.equal(T.historyOp('/t/p/a', '/t/p/a', true), 'none'); // l on the thread already open
  assert.equal(T.historyOp('/', '/', false), 'none');
  assert.equal(T.historyOp('/t/p/a', '', true), 'none');
});

test('restoreIndex: same thread if still listed, else same slot', () => {
  const rows = [{ thread: 'a', account: 'p' }, { thread: 'b', account: 'p' }, { thread: 'b', account: 'w' }];
  assert.equal(T.restoreIndex(rows, { thread: 'b', account: 'w', index: 0 }), 2);
  assert.equal(T.restoreIndex(rows, { thread: 'b', index: 2 }), 1); // no account: first match
  assert.equal(T.restoreIndex(rows, { thread: 'gone', account: 'p', index: 1 }), 1);
  assert.equal(T.restoreIndex(rows, { thread: 'gone', index: 9 }), 2); // past the end: last row
  assert.equal(T.restoreIndex(rows, { index: -1 }), 0);
  assert.equal(T.restoreIndex(rows, null), 0);
  assert.equal(T.restoreIndex([], { thread: 'a', index: 0 }), -1);
  assert.equal(T.SPLIT_CH, 140);
});

test('paneView: the entry\'s own list first, then the tab\'s last list, vetted', () => {
  assert.equal(T.paneView({ view: '/starred' }, '/'), '/starred');
  assert.equal(T.paneView({ view: '/' }, '/trash'), '/'); // the entry says inbox: inbox
  assert.equal(T.paneView({}, '/trash'), '/trash');
  assert.equal(T.paneView(null, '/search?q=x'), '/search?q=x');
  assert.equal(T.paneView({ view: '//evil.example/' }, '/all'), '/all');
  assert.equal(T.paneView({ view: '/t/p/a' }, null), '/');
  assert.equal(T.paneView(undefined, undefined), '/');
});

test('refreshStale: any key, write, or write in flight since the request', () => {
  const sent = { lastKey: 100, lastTag: 50, inflight: 0 };
  assert.equal(T.refreshStale(sent, { lastKey: 100, lastTag: 50, inflight: 0 }), false);
  assert.equal(T.refreshStale(sent, { lastKey: 180, lastTag: 50, inflight: 0 }), true); // j during the fetch
  assert.equal(T.refreshStale(sent, { lastKey: 100, lastTag: 90, inflight: 0 }), true); // a write landed
  assert.equal(T.refreshStale(sent, { lastKey: 100, lastTag: 50, inflight: 1 }), true);
});

test('rgbHex: channels to #rrggbb, clamped', () => {
  assert.equal(T.rgbHex(45, 53, 59), '#2d353b');
  assert.equal(T.rgbHex(0, 15, 255), '#000fff');
  assert.equal(T.rgbHex(-4, 300, 7.6), '#00ff08');
  assert.match(T.rgbHex(1, 2, 3), /^#[0-9a-f]{6}$/);
});

test('cycle: Tab and Shift+Tab toggle between the two panes', () => {
  assert.equal(T.cycle('list', 1), 'thread');
  assert.equal(T.cycle('thread', 1), 'list');
  assert.equal(T.cycle('list', -1), 'thread');
  assert.equal(T.cycle('thread', -1), 'list');
  assert.equal(T.cycle(null, 1), 'list');
  assert.equal(T.cycle('compose', -1), 'list');
});

test('position: count alone on one page, range and total when paged (read.go positionText)', () => {
  assert.equal(T.position(0, 12, 12, false), '12');
  assert.equal(T.position(0, 0, 0, false), '0');
  assert.equal(T.position(50, 50, 44095, true), '51–100 of 44,095');
  assert.equal(T.position(1000, 49, -1, true), '1,001–1,049');
  assert.equal(T.position(50, 0, 50, true), '0');
});
