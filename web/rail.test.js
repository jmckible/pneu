// node --test web/rail.test.js — the pure parts of the header rail and key
// bar (web/static/rail.js). Lives outside web/static so the embed doesn't
// serve it.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const R = require('./static/rail.js');

test('stationOf: views by path (server.go navViews), a search only with a query', () => {
  assert.equal(R.stationOf('/'), '1');
  assert.equal(R.stationOf('/?page=2'), '1');
  assert.equal(R.stationOf('http://pneu.localhost:7317/starred'), '2');
  assert.equal(R.stationOf('/sent'), '3');
  assert.equal(R.stationOf('/all'), '4');
  assert.equal(R.stationOf('/spam'), '5');
  assert.equal(R.stationOf('/trash'), '6');
  assert.equal(R.stationOf('/search?q=cabin'), 'q');
  assert.equal(R.stationOf('/search?q=%20'), null);
  assert.equal(R.stationOf('/search'), null);
  assert.equal(R.stationOf('/t/personal/000000000000000e'), null);
  assert.equal(R.stationOf('/compose'), null);
});

test('animates: only a trip between two different stations', () => {
  assert.equal(R.animates('1', '2'), true);
  assert.equal(R.animates('2', 'q'), true);
  assert.equal(R.animates('1', '1'), false, 'paging, or the same view again');
  assert.equal(R.animates(null, '2'), false, 'from a thread or compose');
  assert.equal(R.animates('2', null), false, 'to a thread or compose');
  assert.equal(R.animates('2', 'x'), false);
});

test('hop: only between the main line (search included) and the siding', () => {
  assert.equal(R.hop('1', '4'), false);
  assert.equal(R.hop('4', 'q'), false);
  assert.equal(R.hop('4', '5'), true);
  assert.equal(R.hop('6', '1'), true);
  assert.equal(R.hop('q', '6'), true);
  assert.equal(R.hop('5', '6'), false);
});

test('flight: longer trips last longer and stretch more, within bounds', () => {
  assert.deepEqual(R.flight(0), { dur: 300, stretch: 1 });
  assert.deepEqual(R.flight(-110), { dur: 366, stretch: 2 });
  assert.deepEqual(R.flight(5000), { dur: 600, stretch: 2.8 });
});

test('spamLight: dark on first sight, lit by newer spam, dark again on a visit', () => {
  // First look: what's there counts as seen.
  assert.deepEqual(R.spamLight('100', null, false, 500), { lit: false, seen: 100 });
  assert.deepEqual(R.spamLight(100, 100, false, 500), { lit: false, seen: null });
  assert.deepEqual(R.spamLight(200, 100, false, 500), { lit: true, seen: null });
  // Visiting marks everything up to now, and a future-dated spam too.
  assert.deepEqual(R.spamLight(200, 100, true, 500), { lit: false, seen: 500 });
  assert.deepEqual(R.spamLight(900, 100, true, 500), { lit: false, seen: 900 });
  assert.deepEqual(R.spamLight(0, 100, false, 500), { lit: false, seen: null });
  assert.equal(R.spamLight(200, NaN, false, 500).lit, false);
});

test('ruler: the cursor row\'s place in the view, like vim', () => {
  assert.equal(R.ruler(0, 2, 20, 20, false), '3 of 20');
  assert.equal(R.ruler(0, 2, 19, 20, false), '3 of 19', 'one page: the rows are the total');
  assert.equal(R.ruler(50, 2, 50, 312, true), '53 of 312');
  assert.equal(R.ruler(1000, 0, 50, 44095, true), '1,001 of 44,095');
  assert.equal(R.ruler(50, 2, 50, -1, true), '53', 'total not counted yet');
  assert.equal(R.ruler(0, -1, 0, 0, false), '');
  assert.equal(R.ruler(0, 0, 0, 0, false), '');
});

test('rollDir: the direction the number changed', () => {
  assert.equal(R.rollDir({ at: 3, total: 20 }, { at: 4, total: 20 }), 1);
  assert.equal(R.rollDir({ at: 4, total: 20 }, { at: 3, total: 20 }), -1);
  assert.equal(R.rollDir({ at: 3, total: 20 }, { at: 3, total: 19 }), -1, 'an archive under the cursor');
  assert.equal(R.rollDir({ at: 3, total: 20 }, { at: 3, total: 20 }), 0);
  assert.equal(R.rollDir(null, { at: 3, total: 20 }), 0);
});

test('learn: a hint is learned on its third use', () => {
  const c = {};
  assert.equal(R.learn(c, 'list:e'), false);
  assert.equal(R.learn(c, 'list:e'), false);
  assert.equal(R.learn(c, 'list:e'), true);
  assert.equal(c['list:e'], 3);
  assert.equal(R.learn(c, 'thread:e'), false, 'per pane');
});

test('parseCounts: anything malformed is no count', () => {
  assert.deepEqual(R.parseCounts('{"list:e":2,"x":"3","y":-1,"z":4}'), { 'list:e': 2, z: 4 });
  assert.deepEqual(R.parseCounts('nope'), {});
  assert.deepEqual(R.parseCounts('[1,2]'), {});
  assert.deepEqual(R.parseCounts(null), {});
});
