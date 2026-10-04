// tube.js — the tube, the left column of views, across page loads, and the
// small rules of the column, the pager row and the key bar (SPEC.md
// "Layout", "Index views", "Key footer").
//
// Switching views is a full navigation, so the capsule on the active
// station travels by a cross-document view transition (app.css
// @view-transition): both pages render it with the same
// view-transition-name and the browser moves it. Only a station-to-station
// navigation animates; anything else (a thread, compose, paging, the same
// view) is skipped on the old page before anything is captured, so it just
// cuts. The old page hands the new one where the capsule was
// (sessionStorage); the new page sets the trip's length and stretch before
// its first frame.
//
// Loaded in <head>, not deferred, after triage.js (the layout thresholds):
// the tube's fold is decided here before anything renders, so a page never
// paints open and then folds, and pagereveal fires before the first render,
// which base.html's <link rel=expect href="#tube" blocking=render> holds
// until the tube is parsed. The pure parts are exported for
// web/tube.test.js.
(function (root) {
  'use strict';

  // The stations top to bottom (server.go navViews): the search station,
  // the main line, then the bins below the drop.
  var STATIONS = ['q', '1', '2', '3', '4', '5', '6'];
  var PATHS = { '/': '1', '/starred': '2', '/sent': '3', '/all': '4', '/spam': '5', '/trash': '6' };

  // stationOf is the station a URL shows: a view's, the search station's
  // for a search with a query, else null (a thread, compose, an empty
  // search).
  function stationOf(url) {
    var u;
    try { u = new URL(url, 'http://x/'); } catch (e) { return null; }
    if (u.pathname === '/search') return (u.searchParams.get('q') || '').trim() ? 'q' : null;
    return Object.prototype.hasOwnProperty.call(PATHS, u.pathname) ? PATHS[u.pathname] : null;
  }

  function queryOf(url) {
    try { return (new URL(url, 'http://x/').searchParams.get('q') || '').trim(); } catch (e) { return ''; }
  }

  // flight is the trip for a move of dy pixels: longer trips take longer
  // and stretch more mid-flight (the approved mock's ride()).
  function flight(dy) {
    var d = Math.abs(dy);
    return { dur: Math.round(Math.min(600, 300 + d * 0.9)), stretch: Math.round((1 + Math.min(1.8, d / 70)) * 1000) / 1000 };
  }

  // animates: a navigation from station `from` to `to` moves the capsule.
  function animates(from, to) {
    return !!from && !!to && from !== to && STATIONS.indexOf(from) >= 0 && STATIONS.indexOf(to) >= 0;
  }

  // cameFrom is the list a thread page keeps lit (SPEC "Layout"): the one
  // it was opened from (app.js FROM_KEY, {list, thread}, when it names this
  // thread), else the tab's last list (pneu.view), else the inbox. Only a
  // URL with a station counts.
  function cameFrom(fromJSON, view, path) {
    var f = null;
    try { f = JSON.parse(fromJSON); } catch (e) { /* none */ }
    if (f && typeof f.list === 'string' && f.thread === path && listy(f.list)) return f.list;
    return typeof view === 'string' && listy(view) ? view : '/';
  }

  function listy(v) { return v.charAt(0) === '/' && v.charAt(1) !== '/' && v.charAt(1) !== '\\' && !!stationOf(v); }

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

  function num(v) { return Number(v).toLocaleString('en-US'); }

  // pager is the pager row's reading (list.html .pager) for the cursor at
  // sel on a page of rows starting at start, of total threads (-1 while
  // uncounted): at, the cursor's place in the whole view; pos and of, "53"
  // and " of 2,318" (of empty while uncounted); range, "51–100"; seg, the
  // page's slice of the track, and dot, the cursor's place on it, in
  // percent (null while uncounted).
  function pager(start, sel, rows, total) {
    var out = { at: 0, pos: '', of: '', range: '', seg: null, dot: null };
    if (!(rows > 0)) return out;
    var at = start + Math.max(sel, 0) + 1;
    out.at = at;
    out.pos = sel >= 0 ? num(at) : '';
    out.range = num(start + 1) + '–' + num(start + rows);
    if (total > 0) {
      total = Math.max(total, start + rows);
      out.of = '\u00a0of ' + num(total);
      out.seg = { left: start / total * 100, width: rows / total * 100 };
      out.dot = sel >= 0 ? (at - 0.5) / total * 100 : null;
    }
    return out;
  }

  // rollDir is the odometer's direction for a change from place a to b: up
  // (1) when it grows, 0 for no change or no earlier place.
  function rollDir(a, b) {
    if (!a || !b || a === b) return 0;
    return b > a ? 1 : -1;
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

  var X = {
    STATIONS: STATIONS, LEARNED: LEARNED, stationOf: stationOf, queryOf: queryOf, flight: flight, animates: animates,
    cameFrom: cameFrom, spamLight: spamLight, pager: pager, rollDir: rollDir, learn: learn, parseCounts: parseCounts,
  };
  if (typeof module === 'object' && module.exports) { module.exports = X; return; }
  var Pneu = (root.Pneu = root.Pneu || {});
  Pneu.tube = X;
  var tri = Pneu.triage || null;
  var html = document.documentElement;

  var reduce = root.matchMedia ? root.matchMedia('(prefers-reduced-motion: reduce)') : null;
  function still() { return !!(reduce && reduce.matches); }

  function session(fn) {
    try { return fn(root.sessionStorage); } catch (e) { return null; }
  }

  // ---- the fold -------------------------------------------------------------
  // The app font's ch, measured on the root (the body isn't parsed yet;
  // app.css gives html the app font), and the window's layout from it
  // (triage.js layout). The fold is set here, before the first render, so
  // the column never animates on load; app.js keeps it with the window.

  function measureCh() {
    var s = document.createElement('span');
    s.textContent = new Array(101).join('0');
    s.style.cssText = 'position:absolute;visibility:hidden;white-space:pre;left:-9999px;top:0';
    html.appendChild(s);
    var w = s.getBoundingClientRect().width / 100;
    s.remove();
    return w > 0 ? w : 8.4;
  }

  var ch = measureCh();

  // layout is the window's {split, fold} now.
  function layout() {
    return tri ? tri.layout(root.innerWidth / ch) : { split: false, fold: false };
  }

  // fold folds the tube (on) or opens it; app.js animates the change once
  // the page has settled (html.tube-anim).
  function fold(on) { html.classList.toggle('tube-fold', !!on); }

  if (tri) {
    html.style.setProperty('--tube-w', tri.TUBE_CH + 'ch');
    html.style.setProperty('--tube-fold-w', tri.TUBE_FOLD_CH + 'ch');
  }
  fold(layout().fold);
  X.ch = ch;
  X.layout = layout;
  X.fold = fold;

  // ---- marking a station ----------------------------------------------------

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function tube() { return document.getElementById('tube'); }

  // searchStation makes the search station show q (a station with the query
  // and a × to leave, whose href is back), or the ghost (q empty), as
  // base.html renders each.
  function searchStation(stop, q, back) {
    var qt = stop.querySelector('.qt');
    if (stop.classList.contains('active') === !!q && (!q || (qt && qt.textContent === q))) return;
    var a = el('a');
    a.appendChild(el('kbd', null, '/'));
    if (q) {
      stop.className = 'q active';
      a.href = '/search?q=' + encodeURIComponent(q);
      a.title = 'Search: ' + q;
      a.setAttribute('aria-current', 'page');
      a.appendChild(el('span', 'qt', q));
      var x = el('a', 'x', '×');
      x.href = back || '/';
      x.title = 'Close search';
      x.setAttribute('aria-label', 'Close search');
      stop.replaceChildren(a, x);
    } else {
      stop.className = 'q ghost';
      a.href = '/search';
      a.title = 'Search (/)';
      a.appendChild(el('span', 'nm', 'search'));
      stop.replaceChildren(a);
    }
  }

  // place lights url's station, as the server does for a list page, with
  // the capsule on it; a URL with no station (compose) lights none. back is
  // where the search station's × goes. Returns the station.
  function place(url, back) {
    var t = tube();
    if (!t) return null;
    var station = url ? stationOf(url) : null;
    var path = String(url || '').split('?')[0];
    var cap = t.querySelector('.cap');
    if (!cap) {
      cap = el('span', 'cap');
      cap.setAttribute('aria-hidden', 'true');
    }
    Array.prototype.forEach.call(t.querySelectorAll('a[data-station]'), function (a) {
      var on = station !== 'q' && a.getAttribute('href') === path;
      a.classList.toggle('active', on);
      if (on) { a.setAttribute('aria-current', 'page'); a.appendChild(cap); } else a.removeAttribute('aria-current');
    });
    var stop = t.querySelector('.q');
    if (stop) {
      // An open field is app.js's; it puts the station back when it closes.
      if (!stop.querySelector('form')) searchStation(stop, station === 'q' ? queryOf(url) : '', back);
      if (station === 'q') stop.appendChild(cap);
    }
    if (!station) cap.remove();
    revealActive();
    return station;
  }

  // reveal scrolls the stations (app.css #tube .line, which scrolls when
  // the window is too short for them) just enough to show el, a station
  // or the search field. Only the stations move, never the page.
  function reveal(el) {
    var sc = el && el.closest && el.closest('#tube .line');
    if (!sc || sc.scrollHeight <= sc.clientHeight) return;
    var r = el.getBoundingClientRect(), b = sc.getBoundingClientRect(), pad = 6;
    if (r.top < b.top + pad) sc.scrollTop -= b.top + pad - r.top;
    else if (r.bottom > b.bottom - pad) sc.scrollTop += r.bottom - (b.bottom - pad);
  }

  // revealActive shows the lit station.
  function revealActive() {
    var t = tube(), cap = t && t.querySelector('.cap');
    reveal(cap && cap.closest('[data-station]'));
  }

  // placeThread keeps the station a thread page came from lit (SPEC
  // "Layout"): the thread page renders with no view of its own.
  function placeThread() {
    if (!/^\/t\/[^/]+\/[^/]+$/.test(location.pathname)) return;
    var t = tube();
    if (!t || t.querySelector('.cap')) return;
    place(cameFrom(session(function (s) { return s.getItem('pneu:from'); }),
      session(function (s) { return s.getItem('pneu.view'); }), location.pathname));
  }

  X.place = place;
  X.placeThread = placeThread;
  X.reveal = reveal;
  X.revealActive = revealActive;

  // ---- the view transition ------------------------------------------------

  var HAND = 'pneu:tube'; // {from, y, at}: the old page's capsule, for the new one

  // capsule is the page's capsule and its station, if one is lit.
  function capsule() {
    var t = tube();
    var cap = t && t.querySelector('.cap');
    var st = cap && cap.closest('[data-station]');
    if (!cap || !st) return null;
    var r = cap.getBoundingClientRect();
    return { el: cap, station: st.dataset.station, y: r.top + r.height / 2 };
  }

  // The old page: only a station-to-station trip keeps the transition.
  // Skipping here, before the capture, leaves every other navigation as it
  // was. A capsule away on the easter egg's trip doesn't travel either.
  root.addEventListener('pageswap', function (e) {
    var vt = e.viewTransition;
    if (!vt) return;
    var cap = capsule();
    var to = e.activation && e.activation.entry ? stationOf(e.activation.entry.url) : null;
    var t = tube();
    if (still() || !cap || !animates(cap.station, to) || (t && (t.classList.contains('away') || t.classList.contains('egging')))) {
      vt.skipTransition();
      return;
    }
    session(function (s) { s.setItem(HAND, JSON.stringify({ from: cap.station, y: cap.y, at: Date.now() })); });
  });

  // The new page: a thread page lights where it came from; then the trip's
  // length and stretch, and the new view's label held muted until about
  // three quarters of the way.
  root.addEventListener('pagereveal', function (e) {
    placeThread();
    revealActive();
    var vt = e.viewTransition;
    var hand = session(function (s) { var v = s.getItem(HAND); s.removeItem(HAND); return v; });
    if (!vt) return;
    try { hand = JSON.parse(hand); } catch (err) { hand = null; }
    var cap = capsule();
    if (still() || !hand || !cap || typeof hand.y !== 'number' || Date.now() - hand.at > 5000 || !animates(hand.from, cap.station)) {
      vt.skipTransition();
      return;
    }
    var f = flight(cap.y - hand.y);
    html.style.setProperty('--cap-dur', f.dur + 'ms');
    html.style.setProperty('--cap-s', String(f.stretch));
    html.classList.add('tube-move', 'tube-hold');
    var lit = setTimeout(function () { html.classList.remove('tube-hold'); }, f.dur * 0.75);
    var done = function () {
      clearTimeout(lit);
      html.classList.remove('tube-move', 'tube-hold');
      html.style.removeProperty('--cap-dur');
      html.style.removeProperty('--cap-s');
    };
    vt.finished.then(done, done);
  });
})(this);
