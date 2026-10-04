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

test('span: the page\'s dates, newest to oldest, the year only when it isn\'t this one', () => {
  const now = new Date(2026, 9, 4, 12);
  const d = (y, m, day, h) => new Date(y, m - 1, day, h || 9);
  assert.equal(X.span(d(2026, 9, 23), d(2026, 8, 14), now), 'Sep 23 – Aug 14');
  assert.equal(X.span(d(2024, 12, 30), d(2024, 11, 2), now), 'Dec 30 – Nov 2, 2024');
  assert.equal(X.span(d(2025, 1, 3), d(2024, 12, 12), now), 'Jan 3, 2025 – Dec 12, 2024');
  assert.equal(X.span(d(2026, 1, 3), d(2025, 12, 12), now), 'Jan 3, 2026 – Dec 12, 2025', 'across years, both, this one too');
  assert.equal(X.span(d(2026, 9, 23, 18), d(2026, 9, 23, 7), now), 'Sep 23', 'one day');
  assert.equal(X.span(d(2023, 5, 1, 18), d(2023, 5, 1, 7), now), 'May 1, 2023');
  assert.equal(X.span(d(2026, 8, 14), d(2026, 9, 23), now), 'Sep 23 – Aug 14', 'newest first whatever the order');
  assert.equal(X.span(null, d(2026, 9, 23), now), '');
  assert.equal(X.span(new Date(NaN), d(2026, 9, 23), now), '');
});

test('timeline: now on the left, the view\'s oldest on the right, linear in time', () => {
  const day = 86400e3, now = 5000 * day;
  const t = X.timeline(now, 0, now - 100 * day, now - 300 * day, now - 200 * day);
  assert.equal(t, null, 'no oldest date: no track');
  const p = X.timeline(now, now - 1000 * day, now - 100 * day, now - 300 * day, now - 200 * day);
  assert.ok(Math.abs(p.seg.left - 10) < 1e-9);
  assert.ok(Math.abs(p.seg.width - 20) < 1e-9);
  assert.ok(Math.abs(p.dot - 20) < 1e-9);
  assert.equal(p.year, new Date(now - 1000 * day).getFullYear());
  assert.equal(X.timeline(now, now - 1000 * day, now - 100 * day, now - 300 * day, null).dot, null, 'no cursor, no dot');
  // A future-dated row sits at now; a row older than the view's oldest
  // stretches the far end to it.
  const f = X.timeline(now, now - 1000 * day, now + 5 * day, now - 2000 * day, now + 5 * day);
  assert.equal(f.seg.left, 0);
  assert.equal(f.seg.left + f.seg.width, 100);
  assert.equal(f.dot, 0);
  assert.equal(f.year, new Date(now - 2000 * day).getFullYear());
  assert.equal(X.timeline(now, now + day, now + day, now + day, null), null, 'nothing in the past');
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
