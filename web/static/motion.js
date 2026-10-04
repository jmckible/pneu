// motion.js — the list's small motions (SPEC.md "Index views", Motion): the
// cursor gliding between rows, a removed row closing its gap (and opening
// again on undo), the star's pop, new mail opening in, the emptied list
// drawing its mark. Motion only catches up: app.js changes the state first
// and calls these after, a key press ends any row still opening or closing
// (settle) before app.js measures, and under prefers-reduced-motion none of
// it runs. freshRows and the stash checks are pure; web/motion.test.js runs
// them under node --test.
(function (root) {
  'use strict';

  var GLIDE = 'cubic-bezier(.2,.7,.3,1)';  // the cursor between rows
  var LEAVE = 'cubic-bezier(.3,.6,.2,1)';  // a removed row
  var OUT = 'cubic-bezier(.2,.8,.2,1)';    // arrivals: new rows, the star's width
  var GLIDE_MS = 110, LEAVE_MS = 280, ENTER_MS = 240, OPEN_MS = 240;
  var WASH_MS = OPEN_MS + 1600;
  // More new rows than this at once is a bulk change (a first pull filling
  // the list, a filter), not mail landing: it renders still.
  var FRESH_MAX = 10;
  var STASH_KEY = 'pneu:rows';
  var STASH_MS = 15000;

  // freshRows: the indexes in next (row keys as rendered now) of rows prev
  // (as rendered before) didn't have. No prev (a first load, another view,
  // another page) or too many new rows: none.
  function freshRows(prev, next, max) {
    if (!prev || !next) return [];
    var had = Object.create(null);
    prev.forEach(function (k) { had[k] = true; });
    var out = [];
    next.forEach(function (k, i) { if (!had[k]) out.push(i); });
    return out.length > (max || FRESH_MAX) ? [] : out;
  }

  // stashed: the keys a list stashed before reloading itself (narrow), if
  // they are this list's and recent.
  function stashed(v, url, now) {
    if (!v || typeof v !== 'object' || v.url !== url || !Array.isArray(v.keys)) return null;
    if (typeof v.at !== 'number' || now - v.at < 0 || now - v.at > STASH_MS) return null;
    return v.keys.filter(function (k) { return typeof k === 'string'; });
  }

  var P = { freshRows: freshRows, stashed: stashed, FRESH_MAX: FRESH_MAX, STASH_MS: STASH_MS };
  if (typeof module === 'object' && module.exports) { module.exports = P; return; }

  var doc = root.document;
  function still() {
    return !root.Element || !root.Element.prototype.animate ||
      !!(root.matchMedia && root.matchMedia('(prefers-reduced-motion: reduce)').matches);
  }
  function px(n) { return n + 'px'; }

  // ---- running row animations ------------------------------------------
  // Each is {anim, done}; done is the end state's cleanup (a left row
  // removed). settle ends them all at once, synchronously, so the layout a
  // key measures is the final one.
  var running = [];
  function track(anim, done) {
    var r = { anim: anim, done: done };
    running.push(r);
    anim.onfinish = function () { end(r); };
    return r;
  }
  function end(r) {
    var i = running.indexOf(r);
    if (i < 0) return;
    running.splice(i, 1);
    r.anim.cancel(); // a filled end state must not outlive it (an undone row comes back)
    if (r.done) r.done();
  }
  function settle() {
    running.slice().forEach(end);
  }

  // box: a row's vertical box, for the keyframes that open or close it.
  function box(el) {
    var cs = root.getComputedStyle(el);
    return { h: el.getBoundingClientRect().height, pt: cs.paddingTop, pb: cs.paddingBottom, bb: cs.borderBottomWidth };
  }
  function shut() { return { height: '0px', paddingTop: '0px', paddingBottom: '0px', borderBottomWidth: '0px' }; }
  function open(b) { return { height: px(b.h), paddingTop: b.pt, paddingBottom: b.pb, borderBottomWidth: b.bb }; }
  function frame(a, extra) {
    var f = {};
    Object.keys(a).forEach(function (k) { f[k] = a[k]; });
    Object.keys(extra).forEach(function (k) { f[k] = extra[k]; });
    return f;
  }

  // ---- the cursor --------------------------------------------------------
  // One element under the rows carries the cursor row's look (the
  // selection background and the accent rail; app.css "motion") and is
  // moved to the cursor row's offset, so a move glides instead of jumping.
  // A change mid-glide retargets from where it is. It is measured, never
  // assumed: rows needn't share a height, and a resize, a font or the pane
  // showing again (ResizeObserver on the list) puts it back on its row.
  // measure: a row's top in its list and its height, unrounded (offsetTop
  // rounds, and a pixel off shows against the row's border). The cursor is
  // placed by top, not transform, so its box snaps to pixels exactly as the
  // row's own background would.
  function measure(row) {
    var r = row.getBoundingClientRect(), o = row.parentNode.getBoundingClientRect();
    return { top: r.top - o.top, h: r.height };
  }

  function Cursor(ol) {
    var el = doc.createElement('li');
    el.className = 'cursor';
    el.setAttribute('aria-hidden', 'true');
    el.hidden = true;
    ol.insertBefore(el, ol.firstChild);
    ol.classList.add('cursored');
    this.el = el;
    this.row = null;
    this.busy = 0;
    this.timer = 0;
    var self = this;
    if (root.ResizeObserver) new root.ResizeObserver(function () { self.relayout(); }).observe(ol);
  }

  // to puts the cursor on row. how: absent, instant; 'glide', a key move;
  // or {ms, ease, top, h, from, h0}: animate over ms to top and height h
  // (default: the row's own) from top `from` and height `h0` (default:
  // where it is), for a move in step with rows opening or closing.
  Cursor.prototype.to = function (row, how) {
    var el = this.el;
    this.row = row || null;
    if (!row) { el.hidden = true; return; }
    var glide = how === 'glide';
    if (glide) how = { ms: GLIDE_MS };
    var ms = how && !el.hidden && !still() ? how.ms : 0;
    el.hidden = false;
    var ease = (how && how.ease) || GLIDE;
    var m = how && how.top != null && how.h != null ? null : measure(row);
    var top = how && how.top != null ? how.top : m.top;
    var h = how && how.h != null ? how.h : m.h;
    if (ms && (how.from != null || how.h0 != null)) {
      el.style.transition = 'none';
      if (how.from != null) el.style.top = px(how.from);
      if (how.h0 != null) el.style.height = px(how.h0);
      void el.offsetHeight;
    }
    el.style.transition = ms ? 'top ' + ms + 'ms ' + ease + ', height ' + ms + 'ms ' + ease : 'none';
    el.style.top = px(top);
    el.style.height = px(h);
    if (!ms) void el.offsetHeight; // the next glide starts from here
    this.busy = glide ? 0 : Date.now() + ms;
  };

  Cursor.prototype.relayout = function () {
    var self = this;
    clearTimeout(this.timer);
    var wait = this.busy - Date.now();
    if (wait > 0 || running.length) { this.timer = setTimeout(function () { self.relayout(); }, Math.max(wait, 0) + 20); return; }
    if (this.row && this.row.isConnected) this.to(this.row);
  };

  // busy: until then the cursor moves in step with rows closing or
  // opening, and the layout's own changes mustn't snap it (relayout).
  // settle ends that motion at once: the rows reach their end state and
  // the cursor its row. A glide alone isn't ended: the next move retargets
  // it from wherever it is.
  Cursor.prototype.settle = function () {
    if (!running.length && Date.now() >= this.busy) return;
    settle();
    this.busy = 0;
    if (this.row && this.row.isConnected && !this.el.hidden) this.to(this.row);
  };

  // ---- a row leaving, and coming back -----------------------------------

  // leave slides a removed row right as it fades, then closes its gap
  // (rows below move up), and removes it. cursor: what to do with the
  // highlight, {row, stay} (the next row moves into it: it stays put) or
  // {row} (it was the last row: the highlight moves up in step), or null
  // (the list is empty now).
  function leave(el, cursor, next) {
    var b = box(el), slot = measure(el).top;
    el.classList.add('leaving');
    var a = el.animate([
      frame(open(b), { opacity: 1, transform: 'none' }),
      frame(open(b), { opacity: 0, transform: 'translateX(28px)', offset: 0.5 }),
      frame(shut(), { opacity: 0, transform: 'translateX(28px)' }),
    ], { duration: LEAVE_MS, easing: LEAVE, fill: 'forwards' });
    track(a, function () { if (el.classList.contains('leaving')) el.remove(); });
    if (!cursor) return;
    if (!next) { cursor.to(null); return; }
    if (next.stay) {
      // The next row is still below, moving up into the slot.
      cursor.to(next.row, { ms: LEAVE_MS, ease: LEAVE, top: slot });
    } else {
      cursor.to(next.row, { ms: LEAVE_MS, ease: GLIDE });
    }
  }

  // enter is leave backwards, shorter: an undone removal opens its gap,
  // then fades in from the right.
  function enter(el, cursor) {
    el.classList.remove('leaving', 'removing');
    var b = box(el);
    var a = el.animate([
      frame(shut(), { opacity: 0, transform: 'translateX(28px)' }),
      frame(open(b), { opacity: 0, transform: 'translateX(28px)', offset: 0.5 }),
      frame(open(b), { opacity: 1, transform: 'none' }),
    ], { duration: ENTER_MS, easing: LEAVE });
    track(a, null);
    if (cursor) cursor.to(el, { ms: ENTER_MS / 2, ease: LEAVE, h0: 0, h: b.h });
  }

  // ---- new mail ----------------------------------------------------------

  // arrive opens new rows (indexes into rows) from nothing to their height,
  // then washes each with the accent. The cursor's row keeps the cursor: it
  // moves down in step with the rows opening above it.
  function arrive(rows, idx, sel, cursor) {
    if (!idx.length || still()) return;
    var row = rows[sel];
    var m = row ? measure(row) : { top: 0, h: 0 }; // final: measured before anything opens
    var top = m.top, h = m.h;
    var above = 0, selNew = false;
    var boxes = idx.map(function (i) {
      var b = box(rows[i]);
      if (i < sel) above += b.h;
      if (i === sel) selNew = true;
      return b;
    });
    idx.forEach(function (i, n) {
      var el = rows[i];
      track(el.animate([frame(shut(), {}), frame(open(boxes[n]), {})], { duration: OPEN_MS, easing: OUT }), null);
      el.classList.add('fresh');
      setTimeout(function () { el.classList.remove('fresh'); }, WASH_MS + 100);
    });
    if (cursor && row && (above || selNew)) {
      cursor.to(row, { ms: OPEN_MS, ease: OUT, top: top, h: h, from: top - above, h0: selNew ? 0 : null });
    }
  }

  // stash keeps a list's rows across the reload a narrow list refreshes
  // with, so the page that loads can tell what's new (unstash).
  function stash(url, keys) {
    try { root.sessionStorage.setItem(STASH_KEY, JSON.stringify({ url: url, keys: keys, at: Date.now() })); } catch (e) { /* no storage: nothing animates */ }
  }
  function unstash(url) {
    var v = null;
    try {
      v = JSON.parse(root.sessionStorage.getItem(STASH_KEY));
      root.sessionStorage.removeItem(STASH_KEY);
    } catch (e) { return null; }
    return stashed(v, url, Date.now());
  }

  // ---- the star ----------------------------------------------------------
  // At rest the star is the subject's ::before (app.css). While it moves, a
  // real one stands in for it (the row is .popping, which hides the
  // ::before), then goes, leaving the ::before exactly where it was.
  var pops = new WeakMap();

  function star(row, on) {
    var subj = row.querySelector('.subject');
    if (!subj || still()) return;
    var prev = pops.get(row);
    if (prev) prev();
    var s = doc.createElement('span');
    s.className = 'star';
    s.setAttribute('aria-hidden', 'true');
    s.textContent = '★ ';
    subj.insertBefore(s, subj.firstChild);
    row.classList.add('popping');
    var sr = s.getBoundingClientRect(), w = sr.width;
    // The glyph (a fallback font's, wider than 1ch) is the pop's centre.
    var g = doc.createRange();
    g.setStart(s.firstChild, 0);
    g.setEnd(s.firstChild, 1);
    var gr = g.getBoundingClientRect();
    var cx = gr.left + gr.width / 2, cy = gr.top + gr.height / 2;
    s.style.transformOrigin = px(cx - sr.left) + ' 55%';
    var anims = [], sparks = [], timer = 0;
    function done() {
      clearTimeout(timer);
      anims.forEach(function (a) { a.cancel(); });
      sparks.forEach(function (p) { p.remove(); });
      s.remove();
      row.classList.remove('popping');
      if (pops.get(row) === done) pops.delete(row);
    }
    pops.set(row, done);
    if (!on) {
      anims.push(s.animate([
        { width: px(w), transform: 'scale(1)', opacity: 1 },
        { width: '0px', transform: 'scale(0)', opacity: 0 },
      ], { duration: 160, easing: 'ease-in', fill: 'forwards' }));
      timer = setTimeout(done, 180);
      return;
    }
    anims.push(s.animate([{ width: '0px' }, { width: px(w) }], { duration: 150, easing: OUT }));
    anims.push(s.animate([
      { transform: 'scale(0) rotate(-30deg)' },
      { transform: 'scale(1.35) rotate(8deg)', offset: 0.55 },
      { transform: 'scale(1) rotate(0)' },
    ], { duration: 360, easing: 'cubic-bezier(.3,.7,.3,1)' }));
    // Sparks burst from the glyph's centre, hosted by the row (the subject
    // clips its overflow).
    var rr = row.getBoundingClientRect();
    var x = cx - rr.left - row.clientLeft, y = cy - rr.top - row.clientTop;
    for (var i = 0; i < 6; i++) {
      var ang = (i / 6) * Math.PI * 2 - Math.PI / 2;
      var p = doc.createElement('span');
      p.className = 'spark';
      p.setAttribute('aria-hidden', 'true');
      p.style.left = px(x);
      p.style.top = px(y);
      row.appendChild(p);
      sparks.push(p);
      anims.push(p.animate([
        { transform: 'translate(0,0) scale(1)', opacity: 1 },
        { transform: 'translate(' + px(Math.cos(ang) * 11) + ',' + px(Math.sin(ang) * 11) + ') scale(0)', opacity: 0 },
      ], { duration: 420, delay: 70, easing: 'cubic-bezier(.2,.8,.3,1)', fill: 'both' }));
    }
    timer = setTimeout(done, 520);
  }

  // ---- the empty list's mark ---------------------------------------------
  // The mark draws itself when the list empties under you. pathLength on
  // this copy's paths only (the header's mark shares the template) makes
  // one dash the whole stroke.
  function draw(svg) {
    if (!svg || still()) return;
    Array.prototype.forEach.call(svg.querySelectorAll('path'), function (p) { p.setAttribute('pathLength', '1'); });
    svg.classList.remove('draw');
    void svg.getBoundingClientRect();
    svg.classList.add('draw');
  }
  function undraw(svg) { if (svg) svg.classList.remove('draw'); }

  P.still = still;
  P.Cursor = Cursor;
  P.leave = leave;
  P.enter = enter;
  P.arrive = arrive;
  P.stash = stash;
  P.unstash = unstash;
  P.star = star;
  P.draw = draw;
  P.undraw = undraw;
  (root.Pneu = root.Pneu || {}).motion = P;
})(this);
