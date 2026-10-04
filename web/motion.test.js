// node --test web/motion.test.js — the pure parts of the list's motion
// (web/static/motion.js): which rows of a refresh are new mail.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const M = require('./static/motion.js');

test('freshRows: rows the previous render did not have, by index', () => {
  const prev = ['work a', 'personal b', 'work c'];
  assert.deepEqual(M.freshRows(prev, ['personal n', 'work a', 'personal b', 'work c']), [0]);
  assert.deepEqual(M.freshRows(prev, ['work a', 'work m', 'personal b', 'work n']), [1, 3]);
});

test('freshRows: the key is account and thread, not the thread alone', () => {
  assert.deepEqual(M.freshRows(['work a'], ['personal a', 'work a']), [0]);
});

test('freshRows: removals and reorders are not new', () => {
  assert.deepEqual(M.freshRows(['a', 'b', 'c'], ['c', 'a']), []);
  assert.deepEqual(M.freshRows(['a', 'b'], []), []);
});

test('freshRows: no previous render (a first load, another view) animates nothing', () => {
  assert.deepEqual(M.freshRows(null, ['a', 'b']), []);
  assert.deepEqual(M.freshRows(undefined, ['a']), []);
});

test('freshRows: an empty list gaining mail opens it', () => {
  assert.deepEqual(M.freshRows([], ['a']), [0]);
});

test('freshRows: a bulk change past the cap is still', () => {
  const next = Array.from({ length: M.FRESH_MAX + 1 }, (_, i) => 'n' + i);
  assert.deepEqual(M.freshRows([], next), []);
  assert.equal(M.freshRows([], next.slice(0, M.FRESH_MAX)).length, M.FRESH_MAX);
  assert.deepEqual(M.freshRows(['x'], ['a', 'b', 'x'], 1), []);
});

test('stashed: only this list, only recent, only strings', () => {
  const now = 1_000_000;
  const v = { url: '/', keys: ['work a', 7, 'personal b'], at: now - 2000 };
  assert.deepEqual(M.stashed(v, '/', now), ['work a', 'personal b']);
  assert.equal(M.stashed(v, '/starred', now), null);
  assert.equal(M.stashed({ ...v, at: now - M.STASH_MS - 1 }, '/', now), null);
  assert.equal(M.stashed({ ...v, at: now + 5 }, '/', now), null); // from the future: a clock jump
  assert.equal(M.stashed({ ...v, keys: 'work a' }, '/', now), null);
  assert.equal(M.stashed(null, '/', now), null);
  assert.equal(M.stashed('x', '/', now), null);
});
