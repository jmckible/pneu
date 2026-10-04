// node --test web/tube.test.js — the pure parts of the tube, the pager row
// and the key bar (web/static/tube.js). Lives outside web/static so the
// embed doesn't serve it.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const X = require('./static/tube.js');

test('stationOf: views by path (server.go navViews), a search only with a query', () => {
  assert.equal(X.stationOf('/'), '1');
  assert.equal(X.stationOf('/?page=2'), '1');
  assert.equal(X.stationOf('http://pneu.localhost:7317/starred'), '2');
  assert.equal(X.stationOf('/sent'), '3');
  assert.equal(X.stationOf('/all'), '4');
  assert.equal(X.stationOf('/spam'), '5');
  assert.equal(X.stationOf('/trash'), '6');
  assert.equal(X.stationOf('/search?q=cabin'), 'q');
  assert.equal(X.stationOf('/search?q=%20'), null);
  assert.equal(X.stationOf('/search'), null);
  assert.equal(X.stationOf('/t/personal/000000000000000e'), null);
  assert.equal(X.stationOf('/compose'), null);
});

test('STATIONS: search at the top, then the main line, the bins below the drop', () => {
  assert.deepEqual(X.STATIONS, ['q', '1', '2', '3', '4', '5', '6']);
});

test('animates: only a trip between two different stations', () => {
  assert.equal(X.animates('1', '2'), true);
  assert.equal(X.animates('2', 'q'), true);
  assert.equal(X.animates('4', '5'), true, 'across the drop, no hop');
  assert.equal(X.animates('1', '1'), false, 'paging, or the same view again');
  assert.equal(X.animates(null, '2'), false, 'from compose');
  assert.equal(X.animates('2', null), false, 'to a thread or compose');
  assert.equal(X.animates('2', 'x'), false);
});

test('flight: longer trips last longer and stretch more, within bounds', () => {
  assert.deepEqual(X.flight(0), { dur: 300, stretch: 1 });
  assert.deepEqual(X.flight(-70), { dur: 363, stretch: 2 });
  assert.deepEqual(X.flight(28), { dur: 325, stretch: 1.4 });
  assert.deepEqual(X.flight(5000), { dur: 600, stretch: 2.8 });
});

test('cameFrom: the list this thread was opened from, else the tab\'s last, else the inbox', () => {
  const from = JSON.stringify({ list: '/sent?page=2', thread: '/t/work/0a' });
  assert.equal(X.cameFrom(from, '/starred', '/t/work/0a'), '/sent?page=2');
  assert.equal(X.cameFrom(from, '/starred', '/t/work/0b'), '/starred', 'opened another thread since');
  assert.equal(X.cameFrom(null, '/search?q=cabin', '/t/work/0a'), '/search?q=cabin');
  assert.equal(X.cameFrom('nope', null, '/t/work/0a'), '/');
  assert.equal(X.cameFrom(JSON.stringify({ list: '//evil.example/', thread: '/t/w/1' }), '/compose', '/t/w/1'), '/');
  assert.equal(X.cameFrom(null, '/search', '/t/w/1'), '/', 'an empty search has no station');
});

test('spamLight: dark on first sight, lit by newer spam, dark again on a visit', () => {
  // First look: what's there counts as seen.
  assert.deepEqual(X.spamLight('100', null, false, 500), { lit: false, seen: 100 });
  assert.deepEqual(X.spamLight(100, 100, false, 500), { lit: false, seen: null });
  assert.deepEqual(X.spamLight(200, 100, false, 500), { lit: true, seen: null });
  // Visiting marks everything up to now, and a future-dated spam too.
  assert.deepEqual(X.spamLight(200, 100, true, 500), { lit: false, seen: 500 });
  assert.deepEqual(X.spamLight(900, 100, true, 500), { lit: false, seen: 900 });
  assert.deepEqual(X.spamLight(0, 100, false, 500), { lit: false, seen: null });
  assert.equal(X.spamLight(200, NaN, false, 500).lit, false);
});

test('pager: the cursor\'s place in the whole view, the page\'s range and its slice of the track', () => {
  const p = X.pager(50, 2, 50, 2318);
  assert.equal(p.at, 53);
  assert.equal(p.pos, '53');
  assert.equal(p.of, '\u00a0of 2,318');
  assert.equal(p.range, '51–100');
  assert.ok(Math.abs(p.seg.left - 50 / 2318 * 100) < 1e-9);
  assert.ok(Math.abs(p.seg.width - 50 / 2318 * 100) < 1e-9);
  assert.ok(Math.abs(p.dot - 52.5 / 2318 * 100) < 1e-9);
  assert.equal(X.pager(1000, 0, 50, 44095).range, '1,001–1,050');
  const u = X.pager(50, 2, 50, -1);
  assert.equal(u.pos, '53');
  assert.equal(u.of, '', 'not counted yet: the place alone');
  assert.equal(u.seg, null);
  assert.equal(u.dot, null);
  assert.equal(X.pager(50, 0, 0, 312).range, '', 'an emptied page');
  assert.equal(X.pager(0, 49, 50, 40).of, '\u00a0of 50', 'a stale total never reads less than the rows shown');
});

test('rollDir: the direction the place changed', () => {
  assert.equal(X.rollDir(3, 4), 1);
  assert.equal(X.rollDir(4, 3), -1);
  assert.equal(X.rollDir(3, 3), 0);
  assert.equal(X.rollDir(0, 3), 0);
});

test('learn: a hint is learned on its third use', () => {
  const c = {};
  assert.equal(X.learn(c, 'list:e'), false);
  assert.equal(X.learn(c, 'list:e'), false);
  assert.equal(X.learn(c, 'list:e'), true);
  assert.equal(c['list:e'], 3);
  assert.equal(X.learn(c, 'thread:e'), false, 'per pane');
});

test('parseCounts: anything malformed is no count', () => {
  assert.deepEqual(X.parseCounts('{"list:e":2,"x":"3","y":-1,"z":4}'), { 'list:e': 2, z: 4 });
  assert.deepEqual(X.parseCounts('nope'), {});
  assert.deepEqual(X.parseCounts('[1,2]'), {});
  assert.deepEqual(X.parseCounts(null), {});
});
