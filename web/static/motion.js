// motion.js — the list's small motions (SPEC.md "Index views", Motion): the
// cursor gliding between rows. Motion only catches up: app.js changes the
// state first and calls these after, a key press ends any row still opening
// or closing (settle) before app.js measures, and under
// prefers-reduced-motion none of it runs.
(function (root) {
  'use strict';

  var GLIDE = 'cubic-bezier(.2,.7,.3,1)';  // the cursor between rows
  var GLIDE_MS = 110;

  var P = {};
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

  P.still = still;
  P.Cursor = Cursor;
  (root.Pneu = root.Pneu || {}).motion = P;
})(this);
