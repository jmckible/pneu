// rail.js — the header rail across page loads, and the small rules of the
// header and key bar (SPEC.md "Layout", "Key footer").
//
// Switching views is a full navigation, so the capsule under the active
// view travels by a cross-document view transition (app.css @view-transition):
// both pages render it with the same view-transition-name and the browser
// moves it. Only a station-to-station navigation animates; anything else
// (a thread, compose, paging, the same view) is skipped on the old page
// before anything is captured, so it just cuts. The old page hands the
// new one where the capsule was (sessionStorage); the new page decides the
// hop and the trip's length before its first frame.
//
// Loaded in <head>, not deferred: pagereveal fires before the first
// render, and base.html's <link rel=expect href="#rail" blocking=render>
// holds that render until the rail is parsed. The pure parts are exported
// for web/rail.test.js.
(function (root) {
  'use strict';

  // The stations in rail order (server.go navViews): the main line, the
  // search stop at its end, then the siding.
  var STATIONS = ['1', '2', '3', '4', 'q', '5', '6'];
  var SIDING = { 5: true, 6: true };
  var PATHS = { '/': '1', '/starred': '2', '/sent': '3', '/all': '4', '/spam': '5', '/trash': '6' };

  // stationOf is the station a URL shows: a view's, the search stop's for a
  // search with a query, else null (a thread, compose, an empty search).
  function stationOf(url) {
    var u;
    try { u = new URL(url, 'http://x/'); } catch (e) { return null; }
    if (u.pathname === '/search') return (u.searchParams.get('q') || '').trim() ? 'q' : null;
    return Object.prototype.hasOwnProperty.call(PATHS, u.pathname) ? PATHS[u.pathname] : null;
  }

  // hop: the capsule lifts 5px only when it crosses between the main line
  // (the search stop included) and the siding.
  function hop(from, to) {
    return !!SIDING[from] !== !!SIDING[to];
  }

  // flight is the trip for a move of dx pixels: longer trips take longer
  // and stretch more mid-flight (the mock's numbers).
  function flight(dx) {
    var d = Math.abs(dx);
    return { dur: Math.round(Math.min(600, 300 + d * 0.6)), stretch: 1 + Math.min(1.8, d / 110) };
  }

  // animates: a navigation from station `from` to `to` moves the capsule.
  function animates(from, to) {
    return !!from && !!to && from !== to && STATIONS.indexOf(from) >= 0 && STATIONS.indexOf(to) >= 0;
  }

  // spamLight decides the Spam station's light. newest is the newest spam's
  // date (unix seconds, 0 none), seen the last visit's mark (null: never),
  // here whether Spam is showing, now the time in seconds. Returns {lit,
  // seen}: seen is the mark to store (unchanged when null is returned).
  // A first look marks what's there as seen, so old spam doesn't light it;
  // a visit marks everything up to now (a future-dated spam included).
  function spamLight(newest, seen, here, now) {
    newest = Number(newest) || 0;
    if (here) return { lit: false, seen: Math.max(now, newest, seen || 0) };
    if (seen === null || seen === undefined || isNaN(seen)) return { lit: false, seen: newest };
    return { lit: newest > seen, seen: null };
  }

  // ruler is the key bar's position counter, like vim's: the cursor row's
  // place in the whole view ("53 of 312"). start is the page's offset, sel
  // the cursor's index on it, rows the rows on it, total the view's count
  // (-1 unknown: then only the place). Empty for an empty list.
  function ruler(start, sel, rows, total, paged) {
    if (!(rows > 0) || sel < 0) return '';
    var n = function (v) { return Number(v).toLocaleString('en-US'); };
    var at = (paged ? start : 0) + sel + 1;
    if (!paged) total = rows;
    return total >= 0 ? n(at) + ' of ' + n(total) : n(at);
  }

  // rollDir is the odometer's direction for a change from a to b, each
  // {at, total}: up (1) when the place or, at the same place, the total
  // grows; 0 for no change.
  function rollDir(a, b) {
    if (!a || !b) return 0;
    if (b.at !== a.at) return b.at > a.at ? 1 : -1;
    if (b.total !== a.total) return b.total > a.total ? 1 : -1;
    return 0;
  }

  // LEARNED: a hint leaves the key bar once its key has been used this
  // many times.
  var LEARNED = 3;

  // learn counts one use of a hint (id) in counts (a plain object, from
  // localStorage) and says whether it is now learned.
  function learn(counts, id) {
    var n = (Number(counts[id]) || 0) + 1;
    counts[id] = n;
    return n >= LEARNED;
  }

  // parseCounts reads the stored counts; anything malformed is none.
  function parseCounts(json) {
    var v;
    try { v = JSON.parse(json); } catch (e) { return {}; }
    if (!v || typeof v !== 'object' || Array.isArray(v)) return {};
    var out = {};
    Object.keys(v).forEach(function (k) { if (typeof v[k] === 'number' && v[k] > 0) out[k] = v[k]; });
    return out;
  }

  var R = {
    STATIONS: STATIONS, LEARNED: LEARNED, stationOf: stationOf, hop: hop, flight: flight, animates: animates,
    spamLight: spamLight, ruler: ruler, rollDir: rollDir, learn: learn, parseCounts: parseCounts,
  };
  if (typeof module === 'object' && module.exports) { module.exports = R; return; }
  (root.Pneu = root.Pneu || {}).rail = R;

  // ---- the view transition ------------------------------------------------

  var HAND = 'pneu:rail'; // {from, x, at}: the old page's capsule, for the new one
  var reduce = root.matchMedia ? root.matchMedia('(prefers-reduced-motion: reduce)') : null;
  function still() { return !!(reduce && reduce.matches); }

  function session(fn) {
    try { return fn(root.sessionStorage); } catch (e) { return null; }
  }

  // capsule is the page's capsule and its station, if a view is active.
  function capsule() {
    var cap = document.querySelector('#rail .cap');
    var st = cap && cap.closest('[data-station]');
    if (!cap || !st) return null;
    var r = cap.getBoundingClientRect();
    return { el: cap, station: st.dataset.station, x: r.left + r.width / 2 };
  }

  // The old page: only a station-to-station trip keeps the transition.
  // Skipping here, before the capture, leaves every other navigation as it
  // was. A capsule away on the easter egg's trip doesn't travel either.
  root.addEventListener('pageswap', function (e) {
    var vt = e.viewTransition;
    if (!vt) return;
    var cap = capsule();
    var to = e.activation && e.activation.entry ? stationOf(e.activation.entry.url) : null;
    var nav = document.getElementById('rail');
    if (still() || !cap || !animates(cap.station, to) || (nav && nav.classList.contains('away'))) {
      vt.skipTransition();
      return;
    }
    session(function (s) { s.setItem(HAND, JSON.stringify({ from: cap.station, x: cap.x, at: Date.now() })); });
  });

  // The new page: the trip's length and stretch, the hop, and the new
  // view's label held muted until about three quarters of the way.
  root.addEventListener('pagereveal', function (e) {
    var vt = e.viewTransition;
    var hand = session(function (s) { var v = s.getItem(HAND); s.removeItem(HAND); return v; });
    if (!vt) return;
    try { hand = JSON.parse(hand); } catch (err) { hand = null; }
    var cap = capsule();
    if (still() || !hand || !cap || typeof hand.x !== 'number' || Date.now() - hand.at > 5000 || !animates(hand.from, cap.station)) {
      vt.skipTransition();
      return;
    }
    var html = document.documentElement;
    var f = flight(cap.x - hand.x);
    html.style.setProperty('--cap-dur', f.dur + 'ms');
    html.style.setProperty('--cap-s', String(f.stretch));
    html.classList.add('rail-move', 'rail-hold');
    if (hop(hand.from, cap.station)) html.classList.add('rail-hop');
    var lit = setTimeout(function () { html.classList.remove('rail-hold'); }, f.dur * 0.75);
    var done = function () {
      clearTimeout(lit);
      html.classList.remove('rail-move', 'rail-hold', 'rail-hop');
      html.style.removeProperty('--cap-dur');
      html.style.removeProperty('--cap-s');
    };
    vt.finished.then(done, done);
  });
})(this);
