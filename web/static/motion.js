// motion.js — the list's small motions (SPEC.md "Index views", Motion): the
// cursor gliding between rows, a removed row closing its gap (and opening
// again on undo). Motion only catches up: app.js changes the state first
// and calls these after, a key press ends any row still opening or closing
// (settle) before app.js measures, and under prefers-reduced-motion none of
// it runs.
(function (root) {
  'use strict';

  var GLIDE = 'cubic-bezier(.2,.7,.3,1)';  // the cursor between rows
  var LEAVE = 'cubic-bezier(.3,.6,.2,1)';  // a removed row
  var GLIDE_MS = 110, LEAVE_MS = 280, ENTER_MS = 240;

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

  P.still = still;
  P.Cursor = Cursor;
  P.leave = leave;
  P.enter = enter;
  (root.Pneu = root.Pneu || {}).motion = P;
})(this);
