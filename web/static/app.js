// app.js — keyboard, list selection, thread rendering, sync refresh.
//
// Message HTML never touches this document: it goes through
// Pneu.renderMailFrame (mailframe.js) into a sandboxed srcdoc frame. Every
// string this file puts into the page goes through textContent.
(function () {
  'use strict';

  var Pneu = (window.Pneu = window.Pneu || {});
  var body = document.body;
  var origin = body.dataset.origin || location.origin;
  var panes = document.getElementById('panes');
  var search = document.querySelector('form.search input[name=q]');
  var lastKey = 0;
  var tri = Pneu.triage || null; // base.html loads triage.js; see `ready` below
  // Client mode's link (link.js): null on a server, whose hello has none.
  var lk = Pneu.link || null;
  var linkInfo = null;

  function storage(fn) {
    try { return fn(window.sessionStorage); } catch (e) { return null; }
  }

  // windowId names this page load to the server (X-Pneu-Window on every
  // write), which echoes it as a `view` event's from: the one event this
  // page may skip, its own write. A hint only; the server trusts nothing
  // on it.
  var windowId = (function () {
    var b = new Uint8Array(16), out = '';
    window.crypto.getRandomValues(b);
    for (var i = 0; i < b.length; i++) out += (b[i] < 16 ? '0' : '') + b[i].toString(16);
    return out;
  })();

  // writeHeaders adds the window id to a write's headers.
  function writeHeaders(h) {
    h = h || {};
    h['X-Pneu-Window'] = windowId;
    return h;
  }
  Pneu.writeHeaders = writeHeaders; // actions.js: POST /unsubscribe

  // pageLabel is where in the view generation doc's panes were rendered
  // (base.html data-epoch/data-gen, read before the query).
  function pageLabel(doc) {
    var b = doc && doc.body;
    return b && Pneu.triage ? Pneu.triage.viewLabel(b.dataset.epoch, b.dataset.gen) : null;
  }

  // ---- panes and cursors --------------------------------------------------
  // A page holds up to two panes inside #panes: L, the thread list, and T,
  // the open thread. Below the split width only the one the URL names
  // (`primary`) shows and the page works as separate navigations. At the
  // split width (initSplit) both show side by side, `focus` says which one
  // the keys drive, and either can be replaced by content fetched from its
  // own page (fetchMain). initList/initThread take the element to drive and
  // drop whatever they drove before.

  // label: the view generation a pane was rendered at; need: what it must
  // be at least, from hello and `view` (onView). A pane behind its need is
  // fetched again.
  var L = { root: document.querySelector('main.list'), items: [], sel: -1, url: location.pathname + location.search, title: document.title, label: null, need: null };
  var T = { root: document.querySelector('main.thread'), items: [], sel: -1, url: null, pending: null, abort: null, label: null, need: null };
  var primary = T.root ? 'thread' : L.root ? 'list' : null;
  var focus = primary;
  var split = false;

  function cur(c) { return c.items[c.sel] || null; }

  function selKey() { return 'pneu:sel:' + L.url; }

  function select(c, i, scroll) {
    if (!c.items.length) return;
    i = Math.max(0, Math.min(c.items.length - 1, i));
    if (c === T && i !== c.sel) cancelActions(); // the cursor moved
    var was = c.items[c.sel];
    if (was) was.classList.remove('selected');
    c.sel = i;
    var el = c.items[i];
    el.classList.add('selected');
    if (c === T) { syncMark(was); syncMark(el); syncKeybar(); } // the highlight shows on the cursor only
    if (c === L) storage(function (s) {
      s.setItem(selKey(), el.dataset.thread || '');
      s.setItem(selKey() + ':i', String(i));
    });
    if (scroll !== false) {
      el.scrollIntoView({ block: c === L ? 'nearest' : 'start' });
    }
  }

  function move(c, delta, then) {
    return function () {
      select(c, c.sel < 0 ? 0 : c.sel + delta);
      if (then) then();
    };
  }

  function setPrimary(kind) {
    primary = kind;
    if (panes) panes.dataset.primary = kind || '';
    syncKeys();
  }

  function setFocus(kind) {
    focus = kind;
    if (panes) panes.dataset.focus = kind || '';
    // Keys typed into a mail frame go to the frame's document; take focus
    // back so the list's keys come from this one.
    var a = document.activeElement;
    if (kind === 'list' && a && a.tagName === 'IFRAME') { a.blur(); window.focus(); }
    // The thread pane holds DOM focus while it has the keys, so Space,
    // arrows and PageDown scroll it natively.
    // (Not when focus is already inside it, e.g. on a link in a mail frame.)
    if (kind === 'thread' && T.root && !T.root.contains(document.activeElement)) T.root.focus({ preventScroll: true });
    if (kind === 'list' && T.root && document.activeElement === T.root) T.root.blur();
    syncKeys();
  }

  // syncKeys shows the footer's key list for the pane that has the keys.
  function syncKeys() {
    var k = activeKind() || (document.querySelector('main.compose') ? 'compose' : '');
    if (k) body.dataset.keys = k;
    else delete body.dataset.keys;
  }

  // scrollThread: j/k on a thread scroll it three lines (the pane in the
  // split, the page when narrow); n/p move between messages.
  function scrollThread(dir) {
    return function () {
      var line = parseFloat(getComputedStyle(body).lineHeight) || 19.6;
      var opts = { top: dir * 3 * line, behavior: 'instant' };
      if (split && T.root) T.root.scrollBy(opts);
      else window.scrollBy(opts);
    };
  }

  // activeKind is the pane the keys drive: the focused one when split, else
  // the one the URL names.
  function activeKind() {
    var k = split ? focus : primary;
    if (k === 'list' && !L.root) k = T.root ? 'thread' : null;
    if (k === 'thread' && !T.root) k = L.root ? 'list' : null;
    return k;
  }

  // ---- list page ----------------------------------------------------------

  // The server sends Referrer-Policy: no-referrer, so document.referrer is
  // always empty; the list a thread was opened from is remembered here.
  var FROM_KEY = 'pneu:from'; // {list: path+query, thread: path}
  // The index view the split pane shows beside a thread opened directly
  // (reload, resize, link): every list page writes its own path+query here.
  var VIEW_KEY = 'pneu.view';

  function storedView() {
    var v = storage(function (s) { return s.getItem(VIEW_KEY); });
    return tri ? tri.listURL(v) : '/';
  }

  function openRow() {
    var row = cur(L);
    if (!row || !row.dataset.url) return;
    if (split) {
      delete noRead[rowId(row.dataset.account, row.dataset.thread)]; // an explicit open reads
      show(row.dataset.url, { history: 'push', focus: 'thread' });
      return;
    }
    storage(function (s) {
      s.setItem(FROM_KEY, JSON.stringify({ list: location.pathname + location.search, thread: row.dataset.url }));
    });
    location.href = row.dataset.url;
  }

  // from returns the remembered list for this thread page, if it opened it.
  function from() {
    var v = null;
    try { v = JSON.parse(storage(function (s) { return s.getItem(FROM_KEY); })); } catch (e) { /* none */ }
    if (!v || typeof v.list !== 'string' || v.list.charAt(0) !== '/' || v.list.charAt(1) === '/') return null;
    v.here = v.thread === location.pathname;
    return v;
  }

  function rowKey(r) { return { thread: r.dataset.thread, account: r.dataset.account }; }

  // initList drives root: the page's own list, or one fetched into the left
  // pane (which replaced the previous element, and its listeners with it).
  // want ({thread, account, index}) places the cursor; without it the cursor
  // this list last had in this tab comes back.
  // A list fetched beside a thread marks its view in the header nav, as the
  // server does for a list page.
  function markNav(url) {
    var path = String(url || '').split('?')[0];
    Array.prototype.forEach.call(document.querySelectorAll('body > header nav a'), function (a) {
      var on = a.getAttribute('href') === path;
      a.classList.toggle('active', on);
      if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    });
  }

  function initList(root, want, scroll) {
    L.root = root;
    L.items = Array.prototype.slice.call(root.querySelectorAll('li.row'));
    L.sel = -1;
    if (!want) {
      // A thread triaged away from its own page is gone: keep its slot.
      var saved = storage(function (s) { return s.getItem(selKey()); });
      var slot = parseInt(storage(function (s) { return s.getItem(selKey() + ':i'); }), 10) || 0;
      want = saved ? { thread: saved, index: slot } : { index: slot };
    }
    var start = tri ? tri.restoreIndex(L.items.map(rowKey), want) : 0;
    select(L, start, scroll !== undefined ? scroll : start > 0);
    whenReady(function () { listTotal(root); })();
    root.addEventListener('click', function (e) {
      var row = e.target.closest('li.row');
      if (!row || e.target.closest('a')) return;
      select(L, L.items.indexOf(row), false);
      openRow(); // a click is an open, as Enter is
    });
  }

  // ---- thread page --------------------------------------------------------

  function loadMailFrame() {
    // mailframe.js is its own file so the hostile harness can load exactly
    // this code; fetch it here if the page didn't include it.
    if (Pneu.renderMailFrame) return Promise.resolve();
    return new Promise(function (resolve, reject) {
      var s = document.createElement('script');
      s.src = '/static/mailframe.js';
      s.onload = resolve;
      s.onerror = reject;
      document.head.appendChild(s);
    });
  }

  function note(container, text) {
    var p = document.createElement('p');
    p.className = 'note';
    p.textContent = text;
    container.appendChild(p);
  }

  // surfaceHex is the card's resolved background as #rrggbb, so a themed
  // mail body paints the surface it sits on (--raised is a color-mix, which
  // mailframe.js's hex-only theme check would refuse). A canvas does the
  // color-space conversion; null (then --bg) if anything is off.
  var surfaceCtx = null;
  function surfaceHex(el) {
    try {
      var c = getComputedStyle(el).backgroundColor;
      if (!surfaceCtx) {
        var cv = document.createElement('canvas');
        cv.width = cv.height = 1;
        surfaceCtx = cv.getContext('2d', { willReadFrequently: true });
      }
      surfaceCtx.clearRect(0, 0, 1, 1);
      surfaceCtx.fillStyle = '#000';
      surfaceCtx.fillStyle = c;
      surfaceCtx.fillRect(0, 0, 1, 1);
      var d = surfaceCtx.getImageData(0, 0, 1, 1).data;
      return d[3] === 255 ? tri.rgbHex(d[0], d[1], d[2]) : null;
    } catch (e) { return null; }
  }

  // frameTheme is the app's colors as of now, for a mail that declares none
  // (mailframe.js validates them and falls back to a light sheet).
  function frameTheme(article) {
    var cs = getComputedStyle(document.documentElement);
    return {
      bg: surfaceHex(article) || cs.getPropertyValue('--bg').trim(), fg: cs.getPropertyValue('--fg').trim(),
      accent: cs.getPropertyValue('--accent').trim(), scheme: cs.colorScheme,
    };
  }

  // paintMail renders mail (renderBody's stash) into its frame, creating it
  // the first time, with the current theme. The frame's keys carry its
  // message's identity (docs/actions.md "Which message"): an action typed
  // inside it acts on this message, whatever the cursor says. Replacing the
  // frame's document cancels an action bound to it.
  function paintMail(article, mail) {
    if (mail.frame && Pneu.actions) Pneu.actions.cancelFor(article);
    var ident = identity(article);
    var built = Pneu.renderMailFrame(mail.html, mail.cids, origin, {
      frame: mail.frame || undefined, partBase: mail.partBase,
      onKeydown: function (e) { onKey(e, ident); }, onKeyup: actionKeyup,
      remoteImages: mail.remoteImages, theme: frameTheme(article),
    });
    mail.frame = built.frame;
    mail.colors = built.colors;
    // A declared action is the chip; without one, the button heuristic
    // guesses once the new document has laid out (its load, below).
    mail.declared = built.action;
    setGuess(article, mail, null);
    setChip(article, built.action);
    if (!mail.onLoad) {
      mail.onLoad = function () { if (!mail.declared) guessLink(article, mail); };
      mail.frame.addEventListener('load', mail.onLoad);
    }
    return built;
  }

  // guessLink runs the button heuristic (actions.js guess) on mail's laid
  // out frame and shows its pick, if any, as a chip marked as a guess, with
  // the link highlighted over the frame (the cursor message's only, by
  // app.css). A frame with no layout yet (its message collapsed before it
  // loaded) is tried again when the message expands.
  function guessLink(article, mail) {
    if (!Pneu.actions || !Pneu.actions.guess || mail.declared || !mail.frame) return;
    var g = Pneu.actions.guess(mail.frame);
    mail.guessPending = !g.layout;
    setGuess(article, mail, g.action);
    setChip(article, g.action);
  }

  // setGuess replaces the highlight of mail's guessed link.
  function setGuess(article, mail, action) {
    if (mail.mark) { mail.mark.stop(); mail.mark = null; }
    var div = article.querySelector('.body[data-kind=html]');
    if (action && action.guess && div) {
      try { mail.mark = Pneu.actions.mark(div, mail.frame, action.guess.anchor, action.guess); } catch (e) { mail.mark = null; }
    }
  }

  function mailOf(article) {
    var div = article && article.querySelector('.body[data-kind=html]');
    return div && div.__mail;
  }

  // syncMark: article's highlight may have started or stopped showing (the
  // cursor, a collapse); its re-validation timer runs only while it shows.
  function syncMark(article) {
    var m = mailOf(article);
    if (m && m.mark) m.mark.sync();
  }

  // syncKeybar shows the thread bar's contextual keys, each only when its
  // key has something to act on: o while the cursor message is expanded
  // with a chip, f while the thread has an attachment the viewer opens (f
  // falls back to the thread's first), X while the cursor message has a
  // List-Unsubscribe header (the server's data-unsub).
  function syncKeybar() {
    var bar = document.getElementById('keybar');
    if (!bar) return;
    var a = T.items && T.items[T.sel];
    var show = {
      o: !!(a && !a.classList.contains('collapsed') && a.querySelector(':scope > header > .cta')),
      f: !!(T.root && T.root.querySelector('.attachments a[data-view]')),
      X: !!(a && a.hasAttribute('data-unsub')),
    };
    Object.keys(show).forEach(function (k) {
      var el = bar.querySelector('[data-ctx="' + k + '"]');
      if (el) el.hidden = !show[k];
    });
  }

  // setChip puts the message's action in its header as the o chip, or
  // takes an old one away: the declared one (mailframe.js, from its
  // JSON-LD), or a guess (guessLink).
  function setChip(article, action) {
    var header = article.querySelector(':scope > header');
    if (!header || !Pneu.actions) return;
    var old = header.querySelector(':scope > .cta');
    if (old) old.remove();
    var c = action ? Pneu.actions.chip(action, function () { openChip(article, null); }) : null;
    if (c) header.appendChild(c);
    syncKeybar();
  }

  // rethemeFrames repaints every loaded body in the new theme's colors. A
  // mail with colors of its own is a light sheet whatever the theme: skipped.
  function rethemeFrames() {
    Array.prototype.forEach.call(document.querySelectorAll('.body[data-kind=html]'), function (div) {
      var m = div.__mail;
      var a = div.closest('article');
      if (m && m.frame && !m.colors && a) paintMail(a, m);
    });
  }

  // reloadTheme: the desktop switched themes (SSE `theme`). A new link with a
  // fresh URL fetches the sheet; the old one goes once it has loaded, so
  // nothing flashes unstyled, then the frames follow.
  var themeSeq = 0;
  function reloadTheme() {
    var seq = ++themeSeq;
    var olds = document.querySelectorAll('link[rel=stylesheet][href^="/theme.css"]');
    var link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = '/theme.css?v=' + Date.now();
    link.addEventListener('load', function () {
      if (seq !== themeSeq) { link.remove(); return; }
      Array.prototype.forEach.call(document.querySelectorAll('link[rel=stylesheet][href^="/theme.css"]'), function (l) {
        if (l !== link) l.remove();
      });
      rethemeFrames();
    });
    link.addEventListener('error', function () { link.remove(); });
    var last = olds[olds.length - 1];
    if (last) last.after(link);
    else document.head.appendChild(link);
  }

  // renderBody loads an HTML body into its frame. The fetch is tied to the
  // thread instance (T.abort), so replacing the pane drops the old loads.
  function renderBody(article) {
    var div = article.querySelector('.body[data-kind=html]');
    if (!div || div.dataset.state) return;
    div.dataset.state = 'loading';
    var signal = T.abort ? T.abort.signal : undefined;
    var account = article.dataset.account || (T.root && T.root.dataset.account);
    Promise.all([
      loadMailFrame(),
      fetch(div.dataset.bodyUrl, { credentials: 'same-origin', headers: { Accept: 'application/json' }, signal: signal }),
    ]).then(function (r) {
      var res = r[1];
      if (res.status === 404) return null; // no HTML part: leave the div empty
      if (!res.ok) throw new Error('HTTP ' + res.status);
      return res.json();
    }).then(function (data) {
      if (!div.isConnected) return; // the pane moved on
      if (!data) { div.dataset.state = 'empty'; return; }
      // This message's own part prefix, byte-for-byte what read.go puts in
      // the cids map (url.PathEscape; data-msgid already carries it escaped).
      // mailframe.js narrows img-src to it and silently widens to /part/ on
      // anything malformed.
      var partBase = '/part/' + encodeURIComponent(account) +
        '/' + article.dataset.msgid + '/';
      // Remote images load, minus receipt pixels (mailframe.js), except in
      // spam and trash (read.go marks those): there an open confirms the
      // address to whoever is guessing at it, so it waits for a click.
      // The body answer carries the tags as of now, so a message tagged
      // while the thread was open doesn't load on a stale page.
      var hold = div.dataset.remoteImages === 'click' || data.hold === true;
      // The div keeps what it takes to paint the frame again: the remote
      // images click, and a theme change (rethemeFrames).
      var mail = div.__mail = {
        html: data.html || '', cids: data.cids || {}, partBase: partBase, remoteImages: !hold, frame: null, colors: false,
      };
      var built = paintMail(article, mail);
      if (hold && built.remote) {
        var btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'remote-images';
        btn.textContent = 'Load remote images';
        btn.addEventListener('click', function () {
          mail.remoteImages = true;
          paintMail(article, mail);
          btn.remove();
        });
        div.appendChild(btn);
      }
      div.appendChild(built.frame);
      div.dataset.state = 'done';
    }).catch(function (err) {
      if (signal && signal.aborted) return;
      div.dataset.state = 'error';
      note(div, 'Could not load this message: ' + err.message);
    });
  }

  function toggle(article) {
    article.classList.toggle('collapsed');
    syncMark(article);
    syncKeybar();
    if (article.classList.contains('collapsed')) return;
    renderBody(article);
    var div = article.querySelector('.body[data-kind=html]'), m = div && div.__mail;
    if (m && m.guessPending) guessLink(article, m);
  }

  // initThread drives root: the page's own thread, or one fetched into the
  // right pane. The previous instance's body loads are aborted; its
  // listeners went with its element.
  function initThread(root) {
    cancelActions(); // the thread re-renders
    // The previous thread's highlights go with it, timers and all.
    (T.items || []).forEach(function (a) {
      if (root.contains(a)) return;
      var m = mailOf(a);
      if (m && m.mark) { m.mark.stop(); m.mark = null; }
    });
    if (T.abort) T.abort.abort();
    T.abort = window.AbortController ? new AbortController() : null;
    T.root = root;
    T.items = Array.prototype.slice.call(root.querySelectorAll('article.message'));
    T.sel = -1;
    leaving = false;
    root.addEventListener('click', function (e) {
      var link = e.target.closest('.attachments a[data-view]');
      if (!link || e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
      if (openViewer(link)) e.preventDefault();
    });
    T.items.forEach(function (a) {
      if (!a.classList.contains('collapsed')) renderBody(a);
      var header = a.querySelector('header');
      if (header) header.addEventListener('click', function (e) {
        if (e.target.closest('a, button')) return;
        select(T, T.items.indexOf(a), false);
        toggle(a);
      });
    });
    var first = root.querySelector('article.message.unread');
    var i = first ? T.items.indexOf(first) : T.items.length - 1;
    if (T.items[i] && T.items[i].classList.contains('collapsed')) toggle(T.items[i]);
    select(T, i);
    if (activeKind() === 'thread' && !root.contains(document.activeElement)) root.focus({ preventScroll: true });
  }

  // u / Esc: in the split, hand the keys back to the list and keep the pane.
  function backToList() {
    if (split && L.root) { setFocus('list'); return; }
    var f = from();
    location.href = f && f.here ? f.list : storedView();
  }

  // ---- pane switching (h, l, Tab) -----------------------------------------
  // l is Enter on the cursor row (open, history, read-on-open), or just the
  // keys when the pane already shows that thread; h is u. Narrow, they
  // navigate the same way Enter and u do. Maximized, they swap which pane
  // fills the window instead.

  function toPane(kind) {
    var now = activeKind();
    if (!now || kind === now) return;
    if (maxed) {
      if (kind === 'thread' ? !T.root : !L.root) return;
      setPrimary(kind);
      setFocus(kind);
      return;
    }
    if (kind === 'list') { backToList(); return; }
    if (split && T.root && !cur(L)) { setFocus('thread'); return; }
    openRow();
  }

  function cycle(dir) { toPane(tri.cycle(activeKind(), dir)); }

  // + maximizes the active pane: the narrow layout, showing that pane, until
  // + again or the window crosses the split width. body.maxed puts a marker
  // in the footer.
  var maxed = false;
  var applySplit = null; // initSplit's apply()

  function toggleMax() {
    if (!applySplit) return;
    if (maxed) {
      var k = activeKind();
      maxed = false;
      body.classList.remove('maxed');
      setPrimary(tri.pathKind(location.pathname + location.search) || primary);
      applySplit();
      if (split && k) setFocus(k);
      return;
    }
    if (!split) return;
    var active = activeKind();
    maxed = true;
    body.classList.add('maxed');
    applySplit();
    setPrimary(active);
  }

  // ---- split pane ---------------------------------------------------------

  var showSeq = 0;
  var listSeq = 0;

  // fetchMain GETs one of our own pages and returns its main.<kind>, adopted
  // into this document, and its title. Server-rendered app markup only: mail
  // HTML never appears in it (bodies load into frames by renderBody).
  function fetchMain(url, kind) {
    return fetch(url, {
      credentials: 'same-origin',
      cache: 'no-store',
      headers: { Accept: 'text/html', 'X-Pneu-Pane': '1' },
    }).then(function (res) {
      if (!res.ok) throw new Error('HTTP ' + res.status);
      return res.text();
    }).then(function (html) {
      var doc = new DOMParser().parseFromString(html, 'text/html');
      var main = doc.querySelector('main.' + kind);
      if (!main) throw new Error('no ' + kind + ' in the page');
      return { main: document.adoptNode(main), title: doc.title, label: pageLabel(doc) };
    });
  }

  // inPane: row's thread is what the pane holds now.
  function inPane(row) {
    return !!(row && T.root && T.root.dataset.thread === row.dataset.thread && T.root.dataset.account === row.dataset.account);
  }

  // shown: row's thread is what the pane is showing or about to show. While
  // show() is fetching, that's T.pending, not T.root.
  function shown(row) {
    if (!row) return false;
    if (T.pending) return row.dataset.url === T.pending;
    return inPane(row);
  }

  // A key dropped while the pane's thread was removed and the next one loads.
  function paneBusy() {
    if (!leaving) return false;
    flash('Loading the next thread…');
    return true;
  }

  // noRead: threads marked unread (U on the row) while the pane was opening
  // them (auto-advance after a removal lands asynchronously); that open's
  // read is skipped so it can't undo the U. An explicit open clears it.
  var noRead = {};
  function rowId(account, thread) { return account + ' ' + thread; }

  // setURL: an open pushes an entry (Back returns to what was there); the
  // pane moving on by itself (auto-advance, emptied pane) replaces it.
  function setURL(url, push) {
    var op = tri.historyOp(location.pathname + location.search, url, push);
    // view: the list this entry's thread was shown beside, for a reload.
    var st = { view: L.root ? L.url : undefined };
    if (op === 'push') history.pushState(st, '', url);
    else if (op === 'replace') history.replaceState(st, '', url);
    setPrimary(tri.pathKind(url));
  }

  // show puts the thread at url into the right pane. opts.history is 'push'
  // (Enter, o, l, Tab, click), 'replace' (auto-advance after a removal, undo
  // re-showing a row) or absent (popstate: the URL is already there);
  // opts.focus moves the keys. The newest call wins.
  function show(url, opts) {
    opts = opts || {};
    var seq = ++showSeq;
    function after() {
      if (opts.history) setURL(url, opts.history === 'push');
      else setPrimary('thread');
      if (opts.focus) setFocus(opts.focus);
    }
    if (T.root && T.url === url) { T.pending = null; leaving = false; after(); return; }
    T.pending = url;
    // A new thread: what the old one had to catch up on is moot, and this
    // fetch starts after every event so far. One arriving before it lands
    // sets need (onView).
    T.need = null;
    fetchMain(url, 'thread').then(function (got) {
      if (seq !== showSeq) return;
      T.pending = null;
      if (!split) { leaving = false; return; } // went narrow meanwhile: the page stays what its URL says
      if (T.root) T.root.replaceWith(got.main);
      else panes.appendChild(got.main);
      T.url = url;
      T.label = got.label;
      initThread(got.main);
      var id = rowId(got.main.dataset.account, got.main.dataset.thread);
      if (noRead[id]) delete noRead[id];
      else readOnOpen();
      if (got.title) document.title = got.title;
      after();
      if (tri.behind(T.label, T.need)) threadChanged();
    }).catch(function (err) {
      if (seq !== showSeq) return;
      T.pending = null;
      // The pane's own thread was removed: don't leave its keys dead.
      if (leaving) { leaving = false; if (split) clearThread(); }
      fail('Open', err);
    });
  }

  // advance opens the cursor row after the open thread was removed; the
  // keys stay where they were.
  function advance() {
    var row = cur(L);
    if (!row || !row.dataset.url) return;
    show(row.dataset.url, { history: 'replace' });
  }

  // closePane: Esc on the list in the split closes the thread pane.
  function closePane() {
    if (split && (T.root || T.pending)) clearThread();
  }

  // clearThread empties the right pane: the URL goes back to the list's.
  function clearThread() {
    cancelActions();
    showSeq++;
    T.pending = null;
    leaving = false;
    if (T.abort) T.abort.abort();
    if (T.root) T.root.remove();
    T.root = null;
    T.items = [];
    T.sel = -1;
    T.url = null;
    T.label = T.need = null;
    syncKeybar();
    setFocus('list');
    setURL(L.url, false);
    document.title = L.title || 'pneu';
  }

  // loadList (re)fills the left pane: the list already there (sync, bfcache,
  // resize) or the remembered view. The scroll offset survives a refresh.
  // want is a cursor placement or a function returning one, read when the
  // answer lands. sent (a refresh) is quiet() as of the request: if keys or
  // tag writes moved since, the answer is stale and onStale runs instead.
  function loadList(want, sent, onStale) {
    var url = L.root ? L.url : tri.paneView(history.state, storage(function (s) { return s.getItem(VIEW_KEY); }));
    var seq = ++listSeq;
    return fetchMain(url, 'list').then(function (got) {
      if (seq !== listSeq) return;
      if (sent && tri.refreshStale(sent, quiet())) { if (onStale) onStale(); return; }
      if (typeof want === 'function') want = want();
      var top = L.root ? L.root.scrollTop : 0;
      if (L.root) L.root.replaceWith(got.main);
      else panes.insertBefore(got.main, panes.firstChild);
      L.url = url;
      L.label = got.label;
      if (got.title) L.title = got.title;
      markNav(url);
      initList(got.main, want, false);
      got.main.scrollTop = top;
      var row = cur(L);
      if (row) row.scrollIntoView({ block: 'nearest' });
      // Rendered before a change this window has since heard of: again.
      if (tri.behind(L.label, L.need)) refreshList();
    }).catch(function (err) {
      if (seq === listSeq) fail('Loading the list', err);
    });
  }

  function quiet() { return { lastKey: lastKey, lastTag: lastTag, inflight: inflight }; }

  // refreshList re-renders the split's list in place once nothing is
  // happening (SSE sync, bfcache restore, an undo whose row isn't here), and
  // tries again if something happened while it was fetching.
  var refreshTimer = 0;
  function refreshList() {
    clearTimeout(refreshTimer);
    if (!split || !L.root) return;
    var wait = Math.max(lastKey, lastTag) + 2000 - Date.now();
    if (inflight > 0) wait = Math.max(wait, 500);
    if (wait > 0) { refreshTimer = setTimeout(refreshList, wait); return; }
    loadList(cursorWant, quiet(), refreshList);
  }

  function cursorWant() {
    var row = cur(L);
    return { thread: row && row.dataset.thread, account: row && row.dataset.account, index: Math.max(L.sel, 0) };
  }

  // page follows the list's Older (dir 1) or Newer (-1) link: > lands on
  // the first row, < on the last, so k/< and j/> read as one walk. The
  // split swaps the pane (a thread open beside it stays); narrow, the page
  // loads with its cursor stored where initList looks for it.
  function page(dir) {
    return function () {
      var a = L.root && L.root.querySelector('nav.pages a[rel=' + (dir > 0 ? 'next' : 'prev') + ']');
      if (!a) { flash(dir > 0 ? 'No older mail' : 'No newer mail'); return; }
      var u = new URL(a.href);
      var url = u.pathname + u.search;
      var index = dir > 0 ? 0 : 1e6; // past any last row; restoreIndex clamps
      if (!split) {
        storage(function (s) {
          s.removeItem('pneu:sel:' + url);
          s.setItem('pneu:sel:' + url + ':i', String(index));
        });
        location.assign(url);
        return;
      }
      L.url = url;
      storage(function (s) { s.setItem(VIEW_KEY, url); });
      if (tri.pathKind(location.pathname + location.search) === 'list') setURL(url, true);
      else history.replaceState({ view: url }, ''); // the thread's reload shows this page beside it
      loadList({ index: index });
    };
  }

  function threadWant() {
    return T.root ? { thread: T.root.dataset.thread, account: T.root.dataset.account, index: 0 } : null;
  }

  // The split width is SPLIT_CH of the app font's ch, measured once: CSS
  // media queries resolve ch against the browser's default font, not ours.
  function measureCh() {
    var s = document.createElement('span');
    s.textContent = new Array(101).join('0');
    s.style.cssText = 'position:absolute;visibility:hidden;white-space:pre;left:-9999px;top:0';
    document.body.appendChild(s);
    var w = s.getBoundingClientRect().width / 100;
    s.remove();
    return w > 0 ? w : 8.4;
  }

  function initSplit() {
    if (!panes || !primary || !window.matchMedia) return;
    var mq = window.matchMedia('(min-width: ' + Math.ceil(tri.SPLIT_CH * measureCh()) + 'px)');
    function apply() {
      var was = split;
      split = mq.matches && !maxed;
      body.classList.toggle('split', split);
      if (!split) {
        showSeq++; // an in-flight open must not land on the narrow page
        T.pending = null;
        leaving = false;
        syncKeys();
        return;
      }
      if (was) return;
      openEvents();
      setFocus(primary);
      // A thread going wide (or loaded wide) gets its list beside it.
      if (primary === 'thread') loadList(threadWant());
    }
    function change() {
      if (maxed) {
        maxed = false;
        body.classList.remove('maxed');
        setPrimary(tri.pathKind(location.pathname + location.search) || primary);
      }
      apply();
    }
    applySplit = apply;
    if (mq.addEventListener) mq.addEventListener('change', change);
    else mq.addListener(change);
    apply();
  }

  // Back and forward over the split's own entries swap the pane, not the page.
  window.addEventListener('popstate', function () {
    var kind = tri && tri.pathKind(location.pathname);
    if (!split || !kind) { location.reload(); return; }
    if (kind === 'thread') {
      for (var i = 0; i < L.items.length; i++) {
        if (L.items[i].dataset.url === location.pathname) { select(L, i); break; }
      }
      show(location.pathname, {});
      return;
    }
    if (L.root && location.pathname + location.search === L.url) { clearThread(); return; }
    location.reload();
  });

  if (panes) panes.addEventListener('mousedown', function (e) {
    if (!split) return;
    var m = e.target.closest('main');
    if (m && m === L.root) setFocus('list');
    else if (m && m === T.root) setFocus('thread');
  });

  // ---- triage -------------------------------------------------------------
  // POST /tag (internal/web/tag.go is the contract). Requests run one at a
  // time through a queue, so z always sees the id of the action before it
  // and class changes land in keystroke order. Only archive/trash removal is
  // optimistic (so e e e keeps pace); everything else changes the page when
  // the server answers. The undo stack lives in sessionStorage so an action
  // taken on a thread page can be undone from the list it returned to.

  var ready = tri ? Promise.resolve(tri) : new Promise(function (resolve, reject) {
    // base.html doesn't list triage.js yet; fetch it like mailframe.js.
    var s = document.createElement('script');
    s.src = '/static/triage.js';
    s.onload = function () { tri = Pneu.triage; resolve(tri); };
    s.onerror = reject;
    document.head.appendChild(s);
  });
  ready.catch(function () { flash('Could not load triage.js', 'error'); });

  var UNDO_KEY = 'pneu:undo';   // JSON [{id, action, account, thread, prev:{unread, flagged}, at}], oldest first
  var GEN_KEY = 'pneu:gen';     // bumped on every successful write; a restored list reloads when it moved
  var FLASH_KEY = 'pneu:flash'; // {text, at}: a status line that survives leaving a thread

  var LAST_TAG_KEY = 'pneu:lastTag'; // ms; a list opened right after a thread-page action still waits

  var inflight = 0;
  var lastTag = parseInt(storage(function (s) { return s.getItem(LAST_TAG_KEY); }), 10) || 0;
  var queue = Promise.resolve();
  var held = {}; // action id -> {el, index}: rows this page removed, for undo

  function enqueue(fn) {
    var p = queue.then(function () { return ready; }).then(fn);
    queue = p.catch(function () {});
    return p;
  }

  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  // tagError is a failed write's error; err.link is a client daemon's
  // Pneu-Link outcome when the server's answer never came (fail says it).
  function tagError(res, data) {
    var err = new Error((data && data.error) || 'HTTP ' + res.status);
    err.status = res.status;
    err.link = lk ? lk.outcome(res.headers, data) : '';
    return err;
  }

  // tag POSTs one action. A 503 "locked" (a gmi pull holds the Xapian write
  // lock) is retried once after Retry-After; anything else not ok rejects.
  function tag(action, opts) {
    inflight++;
    return ready.then(function () {
      var body = tri.body(action, opts);
      function attempt(retried) {
        return fetch('/tag', {
          method: 'POST',
          credentials: 'same-origin',
          headers: writeHeaders({ 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }),
          body: body,
        }).then(function (res) {
          return res.json().catch(function () { return null; }).then(function (data) {
            if (res.status === 503 && data && data.error === 'locked' && !retried) {
              flash('Mail database busy, retrying…');
              return sleep(tri.retryAfter(res.headers.get('Retry-After'))).then(function () { return attempt(true); });
            }
            if (!res.ok || !data || !data.ok) throw tagError(res, data);
            return data;
          });
        });
      }
      return attempt(false);
    }).then(function (data) {
      storage(function (s) { s.setItem(GEN_KEY, String((parseInt(s.getItem(GEN_KEY), 10) || 0) + 1)); });
      if (data.epoch && data.gen) applied(data.epoch, data.gen);
      return data;
    }).finally(function () {
      inflight--;
      lastTag = Date.now();
      storage(function (s) { s.setItem(LAST_TAG_KEY, String(lastTag)); });
      if (!inflight) settleViews();
    });
  }

  // appliedGens: the writes this page made whose answers it applied, by
  // tri.appliedKey; their `view` events are skipped (onView). Bounded:
  // an event comes within moments of its answer.
  var appliedGens = {};
  var appliedOrder = [];
  function applied(epoch, gen) {
    var k = tri.appliedKey(epoch, gen);
    appliedGens[k] = true;
    appliedOrder.push(k);
    if (appliedOrder.length > 200) delete appliedGens[appliedOrder.shift()];
  }

  function gen() { return storage(function (s) { return s.getItem(GEN_KEY); }); }

  function loadStack() { return tri.parse(storage(function (s) { return s.getItem(UNDO_KEY); })); }
  function saveStack(st) { storage(function (s) { s.setItem(UNDO_KEY, JSON.stringify(st)); }); }

  // remember records an action the server can undo (resp.id is absent when
  // nothing was written, e.g. unstar with nothing flagged).
  // epoch and gen place the entry for undoConflicts.
  function remember(resp, account, thread, prev) {
    if (!resp.id) return false;
    saveStack(tri.push(loadStack(), {
      id: resp.id, action: resp.action, account: account, thread: thread, prev: prev, at: Date.now(),
      epoch: resp.epoch, gen: resp.gen,
    }));
    return true;
  }

  // ---- status line --------------------------------------------------------

  var statusEl = document.getElementById('status'); // in the key footer (base.html)
  var statusTimer = 0;
  var statusAt = 0;
  var statusKind;

  // carryFlash hands a visible status line to the page about to load.
  function carryFlash() {
    if (statusEl && statusEl.classList.contains('show')) flashLater(statusEl.textContent, statusAt, statusKind);
  }

  function flash(text, kind, action) {
    // Link hints own the line while up; anything else that writes it ends
    // them first, so Enter is never armed for a destination not shown.
    if (Pneu.actions && Pneu.actions.yieldStatus) Pneu.actions.yieldStatus();
    undoShown = false;
    if (!statusEl) {
      statusEl = document.createElement('div');
      statusEl.id = 'status';
      statusEl.setAttribute('role', 'status');
      statusEl.setAttribute('aria-live', 'polite');
      document.body.appendChild(statusEl);
    }
    statusEl.textContent = text;
    // An undoable action gets a button beside the text (the z hint stays for
    // keyboard users); the flash lasts longer so the click has a target.
    if (action && action.label) {
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'undo';
      btn.textContent = action.label;
      btn.addEventListener('click', function () { statusEl.className = ''; action.run(); });
      statusEl.appendChild(btn);
    }
    statusEl.className = 'show' + (kind ? ' ' + kind : '');
    statusAt = Date.now();
    statusKind = kind;
    clearTimeout(statusTimer);
    statusTimer = setTimeout(function () { statusEl.className = ''; }, kind === 'error' ? 6000 : action ? 8000 : 4000);
  }

  Pneu.flash = flash; // compose.js: "Draft restored"

  // statusNode shows node on the status line until statusNode(null): link
  // hints (actions.js) put their prompt and the selected destination here,
  // in full, so the line grows upward while one is up (app.css #status.hint).
  function statusNode(node) {
    if (!statusEl) flash('');
    clearTimeout(statusTimer);
    if (!node) {
      if (statusEl.classList.contains('hint')) { statusEl.className = ''; statusEl.replaceChildren(); }
      return;
    }
    statusEl.replaceChildren(node);
    statusEl.className = 'show hint';
    statusKind = '';
  }

  var undoShown = false; // the line shows an undo toast (done)

  function done(action, undoable) {
    flash(tri.label(action) + (undoable ? ' · z to undo' : ''), '', undoable ? { label: 'Undo', run: undo } : null);
    undoShown = undoable;
  }

  // undoConflict: another window wrote the thread z would undo. z still
  // undoes this window's action (last writer wins); the toast says so.
  function undoConflict() {
    flash('changed elsewhere · z undoes yours anyway', '', { label: 'Undo', run: undo });
    undoShown = true;
  }

  // fail says a write failed. On a client, one that never reached the
  // server says so (safe to press again), and one whose outcome is unknown
  // says the next hello will show the truth (onHello reconciles by gen).
  function fail(what, err) {
    var lt = err && err.link && lk && lk.failText(err.link, linkInfo);
    if (lt) { flash(lt, 'error'); return; }
    flash(what + ' failed: ' + (err && err.message || 'network error'), 'error');
  }

  function flashLater(text, at, kind) {
    storage(function (s) { s.setItem(FLASH_KEY, JSON.stringify({ text: text, at: at || Date.now(), kind: kind })); });
  }

  function flashStored() {
    var v = storage(function (s) { var x = s.getItem(FLASH_KEY); s.removeItem(FLASH_KEY); return x; });
    try { v = JSON.parse(v); } catch (e) { return; }
    if (v && typeof v.text === 'string' && Date.now() - v.at < 4000) flash(v.text, v.kind);
  }

  // ---- list triage --------------------------------------------------------

  function classes(el) {
    return { unread: el.classList.contains('unread'), flagged: el.classList.contains('flagged') };
  }

  // listCount keeps the list's title count (list.html) with the rows: a
  // removal takes one off the page's range and the total, an undo puts it
  // back. data-rows is the row count data-total was counted against.
  function listCount() {
    var n = L.root && L.root.querySelector('.pane-title .n');
    if (!n || !tri) return;
    var rows = L.items.length;
    var total = parseInt(n.dataset.total, 10);
    if (total >= 0) total += rows - (parseInt(n.dataset.rows, 10) || 0);
    n.textContent = tri.position(parseInt(n.dataset.start, 10) || 0, rows, total, 'paged' in n.dataset);
  }

  // listTotal fills in a paged list's total when the server had none cached
  // (read.go total): counting All Mail can take a second, so the page shows
  // first and the count follows.
  function listTotal(root) {
    var n = root.querySelector('.pane-title .n');
    if (!n || !('paged' in n.dataset) || parseInt(n.dataset.total, 10) >= 0) return;
    var url = L.url + (L.url.indexOf('?') < 0 ? '?' : '&') + 'total=1';
    fetch(url, { credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' } })
      .then(function (res) { return res.ok ? res.json() : null; })
      .then(function (d) {
        if (!d || typeof d.total !== 'number' || L.root !== root) return;
        // Counted now: rows triaged away since the render are already out.
        n.dataset.total = String(d.total);
        n.dataset.rows = String(L.items.length);
        listCount();
      })
      .catch(function () {}); // the range alone is still right
  }

  function clearSelection() {
    if (L.items[L.sel]) L.items[L.sel].classList.remove('selected');
    L.sel = -1;
  }

  // takeRow pulls row i out of the list (animated) and selects its successor.
  // In the split, a row that was the open thread hands the pane to the new
  // cursor row, or empties it with the list.
  function takeRow(i) {
    var el = L.items[i];
    var wasShown = split && shown(el);
    clearSelection();
    L.items.splice(i, 1);
    el.classList.add('removing');
    setTimeout(function () { if (el.classList.contains('removing')) el.remove(); }, 150);
    var n = tri.nextIndex(i, L.items.length);
    if (n >= 0) select(L, n);
    listCount();
    if (wasShown) {
      leaving = true; // the pane's thread is gone: its keys wait for the next one
      if (n >= 0) advance();
      else clearThread();
    }
    return { el: el, index: i, shown: wasShown };
  }

  // putRow re-inserts a row taken by takeRow at its old position and selects
  // it; if it was the open thread, it opens again.
  function putRow(h) {
    var i = Math.min(h.index, L.items.length);
    h.el.classList.remove('removing');
    var before = L.items[i] || null;
    var ol = L.root.querySelector('ol');
    if (before) before.parentNode.insertBefore(h.el, before);
    else ol.appendChild(h.el);
    clearSelection();
    L.items.splice(i, 0, h.el);
    select(L, i);
    listCount();
    if (h.shown && split) show(h.el.dataset.url, { history: 'replace' });
  }

  // rowArgs: star and unread act on the row's matched messages (data-msgids);
  // on Starred those are only the flagged ones.
  function rowArgs(row) {
    return { account: row.dataset.account, ids: row.dataset.msgids, thread: row.dataset.thread };
  }

  // Archive, trash and spam act on the whole thread (data-thread-ids), as on the
  // thread page.
  function rowRemoveArgs(row) {
    return tri.removeArgs(row.dataset.account, row.dataset.threadIds || row.dataset.msgids, row.dataset.thread);
  }

  function listRemove(action) {
    var row = cur(L);
    if (!row) return;
    var why = tri.skip(action, L.root.dataset.view);
    if (why) { flash(why); return; }
    var prev = classes(row);
    var h = tri.removes(action, L.root.dataset.view) ? takeRow(L.sel) : null;
    enqueue(function () { return tag(action, rowRemoveArgs(row)); }).then(function (resp) {
      var undoable = remember(resp, row.dataset.account, row.dataset.thread, prev);
      if (h && undoable) held[resp.id] = h;
      done(action, undoable);
    }, function (err) {
      if (h) putRow(h);
      fail(tri.verb(action), err);
    });
  }

  // listToggle runs star/unstar or unread on the selected row. The action is
  // chosen when the request runs, so s s pressed fast stars then unstars.
  function listToggle(pick, unread) {
    var row = cur(L);
    if (!row) return;
    if (unread) noRead[rowId(row.dataset.account, row.dataset.thread)] = true;
    var action;
    enqueue(function () {
      action = pick(row);
      return tag(action, rowArgs(row)).then(function (resp) {
        var prev = classes(row);
        if (action === 'star') row.classList.add('flagged');
        if (action === 'unstar') row.classList.remove('flagged');
        if (action === 'unread') row.classList.add('unread');
        if (inPane(row)) applyChanges(resp.ids, resp.changes, T.items);
        done(action, remember(resp, row.dataset.account, row.dataset.thread, prev));
      });
    }).catch(function (err) { fail(tri.verb(action || 'star'), err); });
  }

  function rowFor(e) {
    for (var i = 0; i < L.items.length; i++) {
      if (L.items[i].dataset.thread === e.thread && L.items[i].dataset.account === e.account) return L.items[i];
    }
    return null;
  }

  function undoOnList(e) {
    var h = held[e.id];
    if (h) { delete held[e.id]; putRow(h); return; }
    var row = rowFor(e);
    if (row && e.prev) {
      row.classList.toggle('unread', !!e.prev.unread);
      row.classList.toggle('flagged', !!e.prev.flagged);
      return;
    }
    // Taken on a thread page, or before a reload: the row isn't here to put back.
    if (!row && tri.removes(e.action, L.root.dataset.view)) {
      if (split) { refreshList(); return; }
      if (primary !== 'list') return;
      flashLater('Undid ' + e.action);
      location.reload();
    }
  }

  // ---- thread triage ------------------------------------------------------

  // Thread actions capture the pane's element and articles when the key is
  // pressed: in the split the pane can hold another thread by the time the
  // queue gets to them.
  var leaving = false;

  function threadArgs(root) {
    return { account: root.dataset.account, ids: root.dataset.msgids, thread: root.dataset.thread };
  }

  // Past the server's id cap archive/trash/spam send the thread id alone.
  function threadRemoveArgs(root) {
    return tri.removeArgs(root.dataset.account, root.dataset.msgids, root.dataset.thread);
  }

  function threadState(arts) {
    return {
      unread: arts.some(function (a) { return a.classList.contains('unread'); }),
      flagged: arts.some(function (a) { return a.classList.contains('flagged'); }),
    };
  }

  // syncRow mirrors a thread's read/star state onto its list row, if listed.
  function syncRow(root, arts) {
    var row = root && rowFor({ thread: root.dataset.thread, account: root.dataset.account });
    if (!row) return;
    var st = threadState(arts);
    row.classList.toggle('unread', st.unread);
    row.classList.toggle('flagged', st.flagged);
  }

  // applyChanges mirrors a response's tag changes onto the articles it named.
  function applyChanges(ids, changes, arts) {
    var set = {};
    (ids || []).forEach(function (id) { set[tri.decodeId(id)] = true; });
    arts.forEach(function (a) {
      if (!set[tri.decodeId(a.dataset.msgid)]) return;
      (changes || []).forEach(function (c) {
        var name = c.slice(1);
        if (name === 'unread' || name === 'flagged') a.classList.toggle(name, c[0] === '+');
      });
    });
  }

  // leave goes back to the list the thread was opened from.
  // history.back() only when the list opened this thread (then it's the
  // previous entry); a restored list reloads itself if stale (pageshow).
  function leave() {
    var f = from();
    if (f && f.here && history.length > 1) history.back();
    else location.href = storedView();
  }

  // threadRemove archives, trashes or spams the open thread. In the split it is the
  // list's action on the thread's row, so the row leaves the list and the
  // pane moves to the next one; a thread the list doesn't show just closes.
  function threadRemove(action) {
    if (!T.root || paneBusy()) return;
    var root = T.root;
    var why = tri.skip(action, root.dataset.in);
    if (why) { flash(why); return; }
    if (split && L.root) {
      var row = rowFor({ thread: root.dataset.thread, account: root.dataset.account });
      if (row) { select(L, L.items.indexOf(row)); listRemove(action); return; }
    }
    leaving = true;
    var prev = threadState(T.items);
    var args = threadRemoveArgs(root);
    var inSplit = split && !!L.root;
    enqueue(function () { return tag(action, args); }).then(function (resp) {
      var undoable = remember(resp, root.dataset.account, root.dataset.thread, prev);
      if (inSplit) {
        if (T.root === root) clearThread();
        done(action, undoable);
        return;
      }
      flashLater(tri.label(action) + (undoable ? ' · z to undo' : ''));
      leave();
    }, function (err) {
      if (T.root === root) leaving = false;
      fail(tri.verb(action), err);
    });
  }

  function threadStar() {
    var root = T.root, arts = T.items;
    if (!root || paneBusy()) return;
    var action;
    enqueue(function () {
      var prev = threadState(arts);
      action = prev.flagged ? 'unstar' : 'star';
      return tag(action, threadArgs(root)).then(function (resp) {
        applyChanges(resp.ids, resp.changes, arts);
        syncRow(root, arts);
        done(action, remember(resp, root.dataset.account, root.dataset.thread, prev));
      });
    }).catch(function (err) { fail(tri.verb(action || 'star'), err); });
  }

  function threadUnread() {
    var root = T.root, arts = T.items, a = cur(T);
    if (!root || !a || paneBusy()) return;
    enqueue(function () {
      var prev = threadState(arts);
      return tag('unread', { account: root.dataset.account, ids: a.dataset.msgid }).then(function (resp) {
        applyChanges(resp.ids, resp.changes, arts);
        syncRow(root, arts);
        done('unread', remember(resp, root.dataset.account, root.dataset.thread, prev));
      });
    }).catch(function (err) { fail('Mark unread', err); });
  }

  // readOnOpen: opening a thread reads what it showed. Not on the undo stack
  // (the server doesn't return an id for it either); z undoes what you did,
  // not what looking did.
  function readOnOpen() {
    var root = T.root, arts = T.items;
    var ids = root && root.dataset.unreadIds;
    if (!ids || !ids.trim()) return;
    var args = { account: root.dataset.account, ids: ids };
    enqueue(function () {
      return tag('read', args);
    }).then(function (resp) {
      applyChanges(resp.ids, resp.changes, arts);
      syncRow(root, arts);
    }, function (err) { fail('Mark read', err); });
  }

  // ---- undo ---------------------------------------------------------------

  function undo() {
    enqueue(function () {
      var p = tri.pop(loadStack());
      var e = p.entry;
      if (!e) { flash('Nothing to undo'); return; }
      saveStack(p.stack);
      return tag('undo', { id: e.id }).then(function (resp) {
        if (L.root) undoOnList(e);
        if (T.root && e.thread === T.root.dataset.thread && e.account === T.root.dataset.account) {
          applyChanges(resp.ids, resp.changes, T.items);
        }
        flash('Undid ' + e.action);
      }, function (err) {
        if (err.status === 409) { flash('Nothing to undo (the server no longer has it)', 'error'); return; }
        saveStack(tri.push(loadStack(), e)); // e.g. still locked: z can try again
        fail('Undo', err);
      });
    });
  }

  function whenReady(fn) {
    return function (e) {
      if (tri) fn(e);
      else ready.then(function () { fn(e); });
    };
  }

  // ---- help ---------------------------------------------------------------

  // Grouped as SPEC.md groups the keys.
  var HELP = [
    ['Views', [
      ['1', 'Inbox'],
      ['2', 'Starred'],
      ['3', 'Sent'],
      ['4', 'Spam'],
      ['5', 'Trash'],
      ['6', 'All Mail'],
    ]],
    ['Move', [
      ['j / k, ↓ / ↑', 'Next / previous row (list) · scroll 3 lines (thread)'],
      ['n / p', 'Next / previous message (thread)'],
      ['g / G', 'First / last row (list)'],
      ['> / <', 'Older / newer page (list)'],
      ['Space, ⇧Space', 'Page down / up (thread)'],
      ['Enter, o', 'Open thread (list) · Enter folds a message (thread)'],
      ['u, Esc', 'Back to the list (Esc first leaves a field) · Esc on the list closes the thread'],
    ]],
    ['Panes', [
      ['h, ←', 'Keys to the list'],
      ['l, →', 'Keys to the thread (opens the cursor row)'],
      ['Tab, ⇧Tab', 'Switch pane (not inside a mail body)'],
      ['+', 'Maximize the active pane · + again restores'],
    ]],
    ['Act', [
      ['e', 'Archive'],
      ['t, #', 'Trash'],
      ['!', 'Mark as spam'],
      ['s', 'Star / unstar'],
      ['U', 'Mark unread'],
      ['z', 'Undo'],
      ['R', 'Sync now'],
      ['r', 'Reply to sender (thread)'],
      ['a', 'Reply all (thread)'],
      ['w, c', 'Write'],
      ['v', 'Open in Gmail (new tab)'],
      ['X', 'Unsubscribe (headers only) · y confirms'],
      ['o', "Open the message's link (shown as a chip)"],
      ['L', "Label the message's links · type a label to select, Enter opens, Esc closes"],
      ['f', 'View attachments (thread) · n/p step, d download, o open in tab, Esc closes'],
      ['/', 'Search'],
      ['?', 'This help · Esc closes'],
    ]],
  ];
  var help = null;

  function toggleHelp() {
    if (!help) {
      help = document.createElement('dialog');
      help.id = 'keys';
      HELP.forEach(function (g) {
        var h = document.createElement('h2');
        h.textContent = g[0];
        var dl = document.createElement('dl');
        g[1].forEach(function (r) {
          var dt = document.createElement('dt');
          var kbd = document.createElement('kbd');
          kbd.textContent = r[0];
          dt.appendChild(kbd);
          var dd = document.createElement('dd');
          dd.textContent = r[1];
          dl.appendChild(dt);
          dl.appendChild(dd);
        });
        help.appendChild(h);
        help.appendChild(dl);
      });
      help.addEventListener('click', function () { help.close(); });
      document.body.appendChild(help);
    }
    if (help.open) { help.close(); return; }
    var info = help.querySelector('.syncinfo');
    if (info) info.replaceWith(syncDetails());
    else help.prepend(syncDetails());
    help.showModal();
  }

  // ---- attachment viewer --------------------------------------------------
  // viewer.js shows one attachment at a time and steps through all of the
  // thread's that the server marked viewable (data-view). f opens the cursor
  // message's first, a plain click the one clicked; the link stays the real
  // /part URL for a modified click.

  function openViewer(link) {
    var links = Array.prototype.slice.call(T.root.querySelectorAll('.attachments a[data-view]'));
    var i = links.indexOf(link);
    if (i < 0 || !Pneu.viewer) return false;
    Pneu.viewer.open({
      items: links.map(function (a) {
        // A link without download= is one /part serves inline (thread.html).
        return { href: a.getAttribute('href'), name: a.textContent, kind: a.dataset.view, inline: !a.hasAttribute('download') };
      }),
      index: i, origin: origin, theme: frameTheme, loadMailFrame: loadMailFrame,
      onClose: function () {
        if (!T.root || !T.root.isConnected) return;
        T.root.focus({ preventScroll: true });
        setFocus('thread');
      },
    });
    return true;
  }

  function viewAttachments() {
    if (!T.root) return;
    var a = cur(T);
    var link = (a && a.querySelector('.attachments a[data-view]')) || T.root.querySelector('.attachments a[data-view]');
    if (!link) { flash('Nothing here to view'); return; }
    openViewer(link);
  }

  // ---- message actions (docs/actions.md) ---------------------------------
  // X, o and L bind to one message when the key is pressed and
  // keeps it: a key from inside a mail frame is that frame's message (its
  // listener carries the identity, paintMail); from the thread pane, the
  // cursor message, which must be expanded. actions.js does the rest.

  // identity is article's message: account, msgid (url.PathEscape'd, as
  // data-msgid carries it), and the elements it lives in, for cancelling.
  function identity(article) {
    var root = article.closest('main.thread');
    return {
      account: article.dataset.account || (root && root.dataset.account),
      msgid: article.dataset.msgid, article: article, root: root,
    };
  }

  // actionTarget is the message a key acts on: the frame's (ident), else the
  // cursor message if expanded; null (with a word why) otherwise.
  function actionTarget(ident) {
    if (ident) return ident.article && ident.article.isConnected && ident.root === T.root ? ident : null;
    var a = cur(T);
    if (!a || !a.dataset.msgid) return null;
    if (a.classList.contains('collapsed')) { flash('Expand the message first (Enter)'); return null; }
    return identity(a);
  }
  Pneu.actionTarget = actionTarget;

  function cancelActions() { if (Pneu.actions) Pneu.actions.cancel(); }
  function actionKeyup(e) { if (Pneu.actions) Pneu.actions.keyup(e); }

  function unsubscribe(e, ident) {
    if (e.repeat || !Pneu.actions || paneBusy()) return;
    var target = actionTarget(ident);
    if (!target) return;
    Pneu.actions.unsubscribe(target, e, {
      flash: flash,
      // linkText words a Pneu-Link outcome ('not-sent', 'unknown') for a
      // write (a POST) or a read; null on a server, or with none.
      linkText: function (out, write) {
        if (!lk || !out) return null;
        return write ? lk.failText(out, linkInfo) : lk.reachText(out, linkInfo);
      },
      archive: function (id) {
        if (!id.root || id.root !== T.root || !id.root.isConnected) { flash('That thread is no longer open'); return; }
        whenReady(function () { threadRemove('archive'); })();
      },
      onClose: function () {
        if (!T.root || !T.root.isConnected) return;
        T.root.focus({ preventScroll: true });
        setFocus('thread');
      },
    });
  }

  // o opens the message's declared link, the one its chip shows: only an
  // expanded message whose chip is rendered (the chip is the preview; no
  // dialog follows), and only while the chip is in view: an o with it
  // scrolled away brings it into view instead, and a later o opens
  // (actions.js primary). The URL is checked again as it opens.
  function primaryLink(e, ident) {
    if (e.repeat || !Pneu.actions || paneBusy()) return;
    var target = actionTarget(ident);
    if (!target) return;
    openChip(target.article, e, target.root);
  }

  // openChip: e is the o keydown, or null for a click on the chip (which
  // is on screen to be clicked).
  function openChip(article, e, root) {
    var c = article.querySelector(':scope > header > .cta');
    if (!c || article.classList.contains('collapsed') || !c.getClientRects().length) {
      flash('No link on this message');
      return;
    }
    var r;
    if (e) {
      var p = Pneu.actions.primary(c, e, paneBounds(root || article.closest('main.thread')));
      if (p.act === 'none') return;
      if (p.act === 'reveal') { flash('The link is in the chip · o again opens it'); return; }
      r = p.check;
    } else {
      r = Pneu.actions.openAction(c);
    }
    if (r.ok) flash('Opened ' + Pneu.actions.destination(r.url));
    else if (r.stale) {
      // A guess that no longer stands opens nothing; guess again, so the
      // chip shows what the body holds now.
      flash('Not opened: ' + r.reason, 'error');
      var div = article.querySelector('.body[data-kind=html]'), m = div && div.__mail;
      if (m) guessLink(article, m);
    } else flash('Refused for safety: ' + r.reason, 'error');
  }

  // paneBounds is what of the thread pane shows: its rect ∩ the viewport,
  // less every piece of app chrome that is sticky or fixed and overlaps it,
  // in either layout: the page header (sticky when narrow), the split's
  // sticky pane titles, the key bar (actions.js usable). o and L both use
  // it.
  function paneBounds(root) {
    var covers = [];
    var chrome = document.querySelectorAll('body > *:not(#hints):not(dialog), main.thread > h1, main.list > .pane-title');
    Array.prototype.forEach.call(chrome, function (el) {
      var pos = getComputedStyle(el).position;
      if ((pos === 'sticky' || pos === 'fixed') && el.getClientRects().length) covers.push(el.getBoundingClientRect());
    });
    return Pneu.actions.usable(root.getBoundingClientRect(),
      { left: 0, top: 0, right: window.innerWidth, bottom: window.innerHeight }, covers);
  }

  // L labels the links of the message's body where they show, over its
  // frame, clipped to the thread pane and the viewport above the key bar.
  function linkHints(e, ident) {
    if (e.repeat || !Pneu.actions || paneBusy()) return;
    var target = actionTarget(ident);
    if (!target) return;
    var frame = target.article.querySelector('.body[data-kind=html] iframe.mail');
    if (!frame || !frame.contentDocument) { flash('No links to label'); return; }
    Pneu.actions.hints(target, e, {
      frame: frame,
      bounds: paneBounds(target.root),
      status: statusNode,
      statusBox: function () { return statusEl; },
      flash: flash,
      onClose: function () {},
    });
  }

  // ---- reply / compose ----------------------------------------------------
  // The compose page itself is compose.js; these only navigate to it.

  function reply(all) {
    return function () {
      var a = cur(T);
      if (!a || !a.dataset.msgid || paneBusy()) return;
      // data-msgid is already url.PathEscape'd by the server.
      location.href = '/reply/' + encodeURIComponent(a.dataset.account || T.root.dataset.account) +
        '/' + a.dataset.msgid + (all ? '?all=1' : '');
    };
  }

  // ---- open in Gmail -----------------------------------------------------
  // v opens the Gmail escape hatch in a new tab: on a list row, /gmail/ (the
  // server redirects to the thread's newest message; the row doesn't carry a
  // Gmail id); on a thread, the selected message's link, or the newest one's.

  function openGmail() {
    var href = null;
    var kind = activeKind();
    if (kind === 'list') {
      var row = cur(L);
      href = row && row.dataset.gmail;
    } else if (kind === 'thread') {
      // The link is a key, not a control: each article carries data-gmail
      // (the newest one is the thread's fallback).
      var art = cur(T);
      if (!art || !art.dataset.gmail) {
        var all = T.root.querySelectorAll('article.message[data-gmail]');
        art = all.length ? all[all.length - 1] : null;
      }
      href = art && art.dataset.gmail;
    }
    if (!href) { flash('No Gmail link for this thread', 'error'); return; }
    window.open(href, '_blank', 'noopener');
  }

  // compose opens a blank message from the open thread's account (the
  // split's pane counts); a list mixes accounts, so the server picks its first.
  function compose() {
    var acct = T.root && T.root.dataset.account;
    location.href = '/compose' + (acct ? '?account=' + encodeURIComponent(acct) : '');
  }

  // ---- keys ---------------------------------------------------------------
  // One map per page kind, then global (consulted after it, and alone on the
  // compose page, whose fields otherwise own the keyboard: Escape blurs,
  // below; Ctrl+Enter sends, compose.js).

  function focusSearch() { if (search) { search.focus(); search.select(); } }

  // VIEWS: 1..6, the header nav's order (server.go navViews).
  var VIEWS = { 1: '/', 2: '/starred', 3: '/sent', 4: '/spam', 5: '/trash', 6: '/all' };

  function goView(e) { location.assign(VIEWS[e.key]); }

  // R: POST /sync queues a sync on every account; the status line says
  // Checking… from the keypress, and the SSE `sync` event re-renders the
  // list if the pull brought anything.
  function syncNow() {
    postSync().catch(function (err) { fail('Sync', err); });
  }

  // postSync asks for a sync and shows Checking… until the server's first
  // word on it (lineAsked).
  function postSync() {
    lineAsk();
    return fetch('/sync', { method: 'POST', credentials: 'same-origin', headers: writeHeaders({ Accept: 'application/json' }) })
      .then(function (res) {
        return res.json().catch(function () { return null; }).then(function (data) {
          if (!res.ok || !data || !data.ok) throw tagError(res, data);
        });
      })
      .catch(function (err) { lineAsked(); throw err; });
  }

  // Coming back to the window asks for a sync, so mail the phone just
  // announced shows up a moment later (an idle sync is about a second)
  // instead of on the next tick; the status line says Checking… meanwhile.
  // A blur that only moved focus into a mail frame isn't leaving: the
  // document still has focus. A launch is `pneu open`'s, over the control
  // socket. At most one request per RETURN_SYNC_GAP; the age refreshes
  // every time.
  var RETURN_SYNC_GAP = 20000;
  var away = false;
  var returnSyncAt = 0;

  function leftWindow() {
    setTimeout(function () {
      if (document.hidden || !document.hasFocus()) away = true;
    }, 0);
  }

  function returnedToWindow() {
    if (!away || document.hidden) return;
    away = false;
    renderLine();
    if (Date.now() - returnSyncAt < RETURN_SYNC_GAP) return;
    returnSyncAt = Date.now();
    postSync().catch(function () { /* the next tick, or R, tries again */ });
  }

  window.addEventListener('blur', leftWindow);
  window.addEventListener('focus', returnedToWindow);
  document.addEventListener('visibilitychange', function () {
    if (document.hidden) leftWindow();
    else returnedToWindow();
  });

  var keys = (Pneu.keys = {
    list: {
      j: move(L, 1),
      k: move(L, -1),
      Escape: closePane,
      Enter: openRow,
      o: openRow,
      g: function () { select(L, 0); },
      G: function () { select(L, L.items.length - 1); },
      '>': whenReady(page(1)),
      '<': whenReady(page(-1)),
      e: whenReady(function () { listRemove('archive'); }),
      '#': whenReady(function () { listRemove('trash'); }),
      t: whenReady(function () { listRemove('trash'); }),
      '!': whenReady(function () { listRemove('spam'); }),
      s: whenReady(function () { listToggle(function (row) { return row.classList.contains('flagged') ? 'unstar' : 'star'; }); }),
      U: whenReady(function () { listToggle(function () { return 'unread'; }, true); }),
      v: openGmail,
    },
    thread: {
      j: scrollThread(1),
      k: scrollThread(-1),
      n: move(T, 1),
      p: move(T, -1),
      u: backToList,
      Escape: backToList,
      Enter: function () { if (cur(T)) toggle(cur(T)); },
      e: whenReady(function () { threadRemove('archive'); }),
      '#': whenReady(function () { threadRemove('trash'); }),
      t: whenReady(function () { threadRemove('trash'); }),
      '!': whenReady(function () { threadRemove('spam'); }),
      s: whenReady(threadStar),
      U: whenReady(threadUnread),
      r: reply(false),
      a: reply(true),
      v: openGmail,
      f: viewAttachments,
      X: unsubscribe,
      o: primaryLink,
      L: linkHints,
    },
    // Consulted after the active pane's map. On a page with no pane
    // (compose) only ANYWHERE applies: its fields and buttons own the rest.
    global: {
      1: goView, 2: goView, 3: goView, 4: goView, 5: goView, 6: goView,
      R: syncNow,
      '?': toggleHelp,
      '/': focusSearch,
      z: whenReady(undo),
      c: compose,
      w: compose,
      h: function () { toPane('list'); },
      l: function () { toPane('thread'); },
      Tab: function (e) { cycle(e.shiftKey ? -1 : 1); },
      '+': toggleMax,
    },
  });
  var ANYWHERE = { 1: true, 2: true, 3: true, 4: true, 5: true, R: true, '?': true };
  // The arrows are hjkl.
  var ARROWS = { ArrowLeft: 'h', ArrowDown: 'j', ArrowUp: 'k', ArrowRight: 'l' };

  Pneu.selection = function () {
    var k = activeKind();
    return k === 'list' ? cur(L) : k === 'thread' ? cur(T) : null;
  };

  // ident: the message whose mail frame the key was typed in (paintMail).
  function onKey(e, ident) {
    lastKey = Date.now();
    // While an X preview loads, or link hints are up, the session takes
    // every key (Esc cancels), so nothing falls through to archive or reply
    // (docs/actions.md).
    if (Pneu.actions && Pneu.actions.pendingKey(e)) return;
    if (help && help.open) {
      // The modal owns the keyboard; Esc closes it natively.
      if (e.key === '?') { e.preventDefault(); help.close(); }
      return;
    }
    if (document.querySelector('dialog[open]')) return; // any other modal owns it too
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return;
    var t = e.target;
    var inFrame = !!(t && t.ownerDocument && t.ownerDocument !== document);
    // A key typed inside a mail frame: the thread pane has the keys.
    if (split && T.root && inFrame) setFocus('thread');
    // Tab inside a mail frame walks its links, as the browser does.
    if (e.key === 'Tab' && inFrame) return;
    if (t && t.nodeType === 1 && t.closest('input, textarea, select, [contenteditable]')) {
      if (e.key === 'Escape') t.blur();
      return;
    }
    var own = Object.prototype.hasOwnProperty;
    var key = own.call(ARROWS, e.key) ? ARROWS[e.key] : e.key;
    var kind = activeKind();
    var map = kind ? keys[kind] : null;
    var fn = map && own.call(map, key) && map[key];
    if (typeof fn !== 'function' && (kind || own.call(ANYWHERE, key))) fn = own.call(keys.global, key) && keys.global[key];
    if (typeof fn !== 'function') return;
    // Enter on a focused link or button keeps its native meaning.
    if (e.key === 'Enter' && t && t.nodeType === 1 && t.closest('a, button')) return;
    e.preventDefault();
    fn(e, ident);
  }

  document.addEventListener('keydown', function (e) { onKey(e); });
  document.addEventListener('keyup', actionKeyup);

  // ---- changes: view generation -------------------------------------------
  // Every write that changes what lists or threads show, from any window, a
  // send, or a sync that pulled something, is a `view` event (view.go); each
  // /events stream opens with `hello`, where the server is now. A list
  // reloads when either says it is behind, unless you were typing or a tag
  // request was in flight or answered in the last two seconds; then it
  // waits for a quiet moment. Selection and the status line survive the
  // reload. In the split the list pane re-renders in place instead
  // (loadList): the cursor stays on its thread, or its slot. The open
  // thread is fetched again when the event names it, in place, keeping its
  // cursor, folds and scroll, or, while you are in the middle of something
  // in it, marked "changed elsewhere" until you're done. This window's own
  // write, once applied from its answer, is skipped: the row it just
  // removed doesn't come back under the cursor. `syncing`/`sync` only
  // drive the status line.

  // One connection per page, every page: `view` and `hello` refresh what is
  // showing, `theme` restyles it.
  var events = null;
  var reloadTimer = 0;
  var heldViews = []; // this window's own events, waiting on its writes (onView)

  // openRef is the thread the pane shows, or is opening.
  function openRef() {
    var m = T.pending && /^\/t\/([^/]+)\/([^/?#]+)/.exec(T.pending);
    if (m) return { account: tri.decodeId(m[1]), thread: m[2] };
    return T.root ? { account: T.root.dataset.account, thread: T.root.dataset.thread } : null;
  }

  function onView(ev) {
    // A thread z would undo was written elsewhere since.
    var stack = loadStack();
    var marked = tri.undoConflicts(stack, ev, windowId);
    if (marked !== stack) {
      saveStack(marked);
      var top = marked[marked.length - 1];
      if (top && top.conflict && !stack[stack.length - 1].conflict && undoShown && statusEl && statusEl.classList.contains('show')) undoConflict();
    }
    var d = tri.viewDecision(ev, { id: windowId, applied: appliedGens, pending: inflight, open: openRef() });
    if (d.act === 'hold') { heldViews.push(ev); return; }
    if (d.act === 'skip') {
      L.label = tri.advance(L.label, ev);
      T.label = tri.advance(T.label, ev);
      return;
    }
    var at = { epoch: ev.epoch, gen: ev.gen };
    needList(at);
    if (d.thread) needThread(at);
  }

  // settleViews decides the held events once this window's writes settle.
  function settleViews() {
    var evs = heldViews;
    heldViews = [];
    evs.forEach(onView);
  }

  // setLink takes a client daemon's link view (hello's, or SSE `link`).
  function setLink(l) {
    if (!lk || !l || typeof l !== 'object') return;
    linkInfo = l;
    renderLine();
    renderAccounts(); // a client's reauth line differs
  }

  function onHello(h) {
    if (h.link) setLink(h.link);
    heldViews = []; // hello counts them
    (h.accounts || []).forEach(accountEvent);
    var at = tri.viewLabel(h.epoch, h.gen);
    if (!at) return;
    needList(at);
    if (openRef()) needThread(at);
  }

  // needList: the list must show at least at; reload it if it doesn't.
  // One still loading beside a thread checks when it lands (loadList).
  function needList(at) {
    if (!L.need || tri.behind(L.need, at)) L.need = at;
    if (L.root && tri.behind(L.label, L.need)) reloadWhenQuiet();
  }

  // needThread: the same for the open thread. One still opening checks
  // when it lands (show).
  function needThread(at) {
    if (!T.need || tri.behind(T.need, at)) T.need = at;
    if (!T.pending && T.root && tri.behind(T.label, T.need)) threadChanged();
  }

  // threadBusy: you are in the middle of something in the thread (a
  // dialog, an unsubscribe, link hints, a text selection) that a re-render
  // would take away.
  function threadBusy() {
    if (Pneu.actions && Pneu.actions.active && Pneu.actions.active()) return true;
    if (document.querySelector('dialog[open]')) return true;
    var sel = window.getSelection && window.getSelection();
    return !!(sel && !sel.isCollapsed && T.root && sel.anchorNode && T.root.contains(sel.anchorNode));
  }

  // threadChanged refetches the open thread now, or marks it changed
  // elsewhere and refetches once you're done.
  var changedTimer = 0;
  function threadChanged() {
    if (!T.root) return;
    if (!threadBusy()) { refetchThread(); return; }
    T.root.dataset.changed = '';
    clearInterval(changedTimer);
    changedTimer = setInterval(function () {
      if (!T.root || !('changed' in T.root.dataset)) { clearInterval(changedTimer); return; }
      if (!threadBusy()) { clearInterval(changedTimer); refetchThread(); }
    }, 1000);
  }

  // refetchThread renders the open thread again in place: new messages
  // come in, tags change, the cursor message, its folds and the scroll stay.
  // An open (show) wins over it. No read-on-open: a message marked unread
  // elsewhere stays unread.
  var refetchSeq = 0;
  function refetchThread() {
    var root = T.root, url = T.url;
    if (!root || !url || T.pending) return;
    var seq = ++refetchSeq, shownAt = showSeq;
    fetchMain(url, 'thread').then(function (got) {
      if (seq !== refetchSeq || shownAt !== showSeq || T.root !== root || T.pending) return;
      if (threadBusy()) { threadChanged(); return; } // something started meanwhile
      var open = {};
      T.items.forEach(function (a) { open[a.dataset.msgid] = !a.classList.contains('collapsed'); });
      var at = cur(T) && cur(T).dataset.msgid;
      var scroller = split ? root : document.scrollingElement;
      var top = scroller ? scroller.scrollTop : 0;
      Array.prototype.forEach.call(got.main.querySelectorAll('article.message'), function (a) {
        if (Object.prototype.hasOwnProperty.call(open, a.dataset.msgid)) a.classList.toggle('collapsed', !open[a.dataset.msgid]);
      });
      root.replaceWith(got.main);
      T.label = got.label;
      initThread(got.main);
      for (var i = 0; i < T.items.length; i++) {
        if (T.items[i].dataset.msgid === at) { select(T, i, false); break; }
      }
      scroller = split ? got.main : document.scrollingElement;
      if (scroller) scroller.scrollTop = top;
      if (tri.behind(T.label, T.need)) threadChanged();
    }).catch(function () {
      // Gone, or the server is: say so, and try again on the next change.
      if (seq === refetchSeq && T.root === root) root.dataset.changed = '';
    });
  }

  function reloadWhenQuiet() {
    clearTimeout(reloadTimer);
    if (split) { refreshList(); return; }
    var wait = Math.max(lastKey, lastTag) + 2000 - Date.now();
    if (inflight > 0) wait = Math.max(wait, 500);
    if (wait <= 0 && document.activeElement !== search) {
      if (primary === 'list') { carryFlash(); location.reload(); }
      return;
    }
    reloadTimer = setTimeout(reloadWhenQuiet, Math.max(wait, 500));
  }

  // ---- accounts -----------------------------------------------------------
  // The #accounts strip under the header: a line for each account that isn't
  // simply working (docs/onboarding.md). data-accounts has every account at
  // load; SSE `account` replaces one as it changes. While an account's first
  // pull runs, the list refreshes every so often as mail lands (newest
  // first), and the account is read-only: the server refuses its writes.

  var acctEl = document.getElementById('accounts');
  var accounts = {};  // name -> the server's account view
  var acctOrder = [];
  var pullRefreshAt = 0;
  var PULL_REFRESH = 20000;

  (function () {
    var list = null;
    try { list = JSON.parse((acctEl && acctEl.dataset.accounts) || 'null'); } catch (err) { /* no strip */ }
    (list || []).forEach(function (a) { accounts[a.name] = a; acctOrder.push(a.name); });
  })();

  function num(n) { return Number(n || 0).toLocaleString('en-US'); }

  // describePull words a first pull's progress as the server does
  // (web.DescribeProgress) and the bar widget does.
  function describePull(p) {
    var out;
    switch (p && p.phase) {
      case 'listing': out = 'listing messages: ' + num(p.done) + ' found'; break;
      case 'removing': out = 'removing deleted messages'; break;
      case 'content': out = 'downloading ' + num(p.done) + ' of ' + num(p.total); break;
      case 'metadata': out = 'checking labels ' + num(p.done) + ' of ' + num(p.total); break;
      default: return 'starting the download';
    }
    if (p.percent != null) out += ' (' + p.percent + '%)';
    if (p.frontier) {
      var d = new Date(p.frontier);
      if (!isNaN(d)) out += ', complete back to ' + d.toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' });
    }
    return out;
  }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function acctButton(label, fn) {
    var b = el('button', null, label);
    b.type = 'button';
    b.addEventListener('click', fn);
    return b;
  }

  function acctPost(name, what) {
    return fetch('/accounts/' + encodeURIComponent(name) + '/' + what, {
      method: 'POST', credentials: 'same-origin', headers: writeHeaders({ Accept: 'application/json' }),
    }).then(function (res) {
      return res.json().catch(function () { return null; }).then(function (data) {
        if (!res.ok || !data || !data.ok) throw tagError(res, data);
        return data;
      });
    });
  }

  // reconnect runs the consent flow. The window opens on the click, before
  // the server answers with Google's URL, or a popup blocker would stop it;
  // the opener link is cut before it leaves for Google.
  function reconnect(name) {
    var w = window.open('', '_blank');
    if (w) { try { w.opener = null; } catch (err) { /* already cut */ } }
    acctPost(name, 'reauth').then(function (data) {
      if (data.connected) { // fixed meanwhile, e.g. `pneu account auth` in a terminal
        if (w) w.close();
        flash(name + ' is already connected to Gmail');
        return;
      }
      if (w) w.location = data.url; else window.open(data.url, '_blank', 'noopener');
      flash('Finish in the Google window; ' + name + ' reconnects when you allow access');
    }, function (err) {
      if (w) w.close();
      var help = lk && lk.reauthRefused(err) && lk.reauthHelp(name, linkInfo);
      if (help) { flash(help.lead + help.command + help.tail, 'error'); return; }
      fail('Reconnect ' + name, err);
    });
  }

  // word: an account name as a suggested command shows it (link.js).
  function word(n) { return Pneu.link ? Pneu.link.accountWord(n) : '<account>'; }

  function acctLine(a) {
    var p = el('p');
    p.dataset.account = a.name;
    p.appendChild(el('span', 'name', a.name));
    p.appendChild(document.createTextNode(': '));
    var text = function (t, cls) { p.appendChild(el('span', cls, t)); };
    switch (a.state) {
      case 'unconfigured':
        text('not set up. Run ');
        p.appendChild(el('code', null, 'pneu account add ' + word(a.name) + ' <address>'));
        break;
      case 'unauthorized':
        text('not connected to Gmail. Run ');
        p.appendChild(el('code', null, 'pneu account auth ' + word(a.name)));
        break;
      case 'needs-pull':
        if (a.failures > 0) {
          text('download stopped' + (a.error ? ' (' + a.error + ')' : '') + '; it retries by itself', 'bad');
          p.appendChild(acctButton('Retry now', function () {
            acctPost(a.name, 'pull').catch(function (err) { fail('Retry', err); });
          }));
        } else {
          text('waiting to download');
        }
        break;
      case 'pulling':
        var m = el('span', 'meter');
        if (a.progress && a.progress.percent != null) m.style.setProperty('--pct', a.progress.percent + '%');
        else m.classList.add('busy');
        p.appendChild(m);
        text(describePull(a.progress) + ' · read-only until it finishes');
        break;
      case 'reauth':
        if (a.authing) {
          text('waiting for you to allow access in the Google window');
          p.appendChild(acctButton('Cancel', function () {
            acctPost(a.name, 'reauth/cancel').catch(function (err) { fail('Cancel', err); });
          }));
        } else if (lk && lk.reauthHelp(a.name, linkInfo)) {
          // A client: consent can't run from this page (409 reauth-on-server).
          var help = lk.reauthHelp(a.name, linkInfo);
          text(help.lead, 'bad');
          p.appendChild(el('code', null, help.command));
          text(help.tail);
        } else {
          text('Gmail access expired or was revoked', 'bad');
          p.appendChild(acctButton('Reconnect', function () { reconnect(a.name); }));
        }
        break;
      default:
        return null; // ready (or no engine): nothing to say
    }
    return p;
  }

  function renderAccounts() {
    if (!acctEl) return;
    var lines = acctOrder.map(function (n) { return acctLine(accounts[n]); }).filter(Boolean);
    acctEl.replaceChildren.apply(acctEl, lines);
  }

  function accountEvent(a) {
    if (!a || !a.name) return;
    var was = accounts[a.name];
    if (!was) acctOrder.push(a.name);
    accounts[a.name] = a;
    renderAccounts();
    lineNews(a);
    // New mail lands newest first while a pull downloads: show it now and then.
    if (a.state === 'pulling' && a.progress && a.progress.phase === 'content' && Date.now() - pullRefreshAt > PULL_REFRESH) {
      pullRefreshAt = Date.now();
      reloadWhenQuiet();
    }
  }

  renderAccounts();

  // ---- status line --------------------------------------------------------
  // The header's far right (SPEC "Sync state"): how current the view is,
  // from each account's sync state (data-accounts at load, SSE `account`
  // after). In order: Checking… while any account's sync is queued or
  // running, or a request (R, focus) has had no answer yet; an account
  // failing; stale; how long ago the stalest account synced. Only ready
  // accounts count: one in its first pull or waiting on setup is the
  // #accounts strip's. Client mode adds link-down and update states here
  // (docs/client.md). A click, or ?, shows the details.

  // An account is failing at the bar's threshold (BarWidget.qml
  // accountSick): any failure.
  var SICK_FAILURES = 1;
  // A queued or running flag with no news this long lost its end event: a
  // reconnect's hello brings every account's view, so only a stream that
  // stays down gets here. A sync is bounded by gmi's own 10m timeout.
  var LINE_STUCK = 11 * 60 * 1000;
  // A request with no answer this long (the server never said queued)
  // stops showing Checking….
  var ASK_WAIT = 10000;
  var AGE_TICK = 30000;
  var SPIN_FRAMES = '⣾⣽⣻⢿⡿⣟⣯⣷';

  var lineEl = document.getElementById('sync');
  var lineText = lineEl && lineEl.querySelector('.text');
  var lineSpin = lineEl && lineEl.querySelector('.spin');
  var lineNudge = lineEl && lineEl.querySelector('.nudge');
  var lineLive = document.getElementById('sync-live');
  var syncEvery = (Number(lineEl && lineEl.dataset.every) || 120) * 1000;
  var busySince = {}; // account -> when it last said queued or running
  var askedAt = 0;    // a POST /sync the server hasn't answered with news
  var askTimer = 0;
  var spinTimer = 0;
  var spinFrame = 0;
  var ageTimer = 0;
  var lineSaid = null; // the last state announced (#sync-live)
  var syncInfo = null; // dialog#syncinfo, once opened
  var workers = 0;     // service worker registrations found on this origin

  // A service worker on pneu's origin (docs/client.md R4): AppCSP's
  // worker-src 'none' means pneu never registers one, so finding one means
  // something else did. Each is unregistered, and the line says to reset
  // the window: a worker that served this page could have served this
  // script too, so this is a tripwire after the reset, not the reset.
  if (navigator.serviceWorker && typeof navigator.serviceWorker.getRegistrations === 'function') {
    navigator.serviceWorker.getRegistrations().then(function (regs) {
      if (!regs || !regs.length) return;
      workers = regs.length;
      regs.forEach(function (r) { try { r.unregister(); } catch (e) { /* the line still says so */ } });
      renderLine();
    }, function () { /* no answer: nothing to say */ });
  }

  acctOrder.forEach(function (n) { if (accounts[n].queued || accounts[n].running) busySince[n] = Date.now(); });

  function readyAccounts() {
    return acctOrder.map(function (n) { return accounts[n]; }).filter(function (a) { return a && a.state === 'ready'; });
  }

  function busy(a) {
    return !!(a.queued || a.running) && Date.now() - (busySince[a.name] || 0) < LINE_STUCK;
  }

  // ago words an age as the line shows it.
  function ago(ms) {
    var m = Math.floor(ms / 60000);
    if (m < 1) return 'just now';
    if (m < 60) return m + 'm ago';
    if (m < 48 * 60) return Math.floor(m / 60) + 'h ago';
    return Math.floor(m / (24 * 60)) + 'd ago';
  }

  // lineState is the line's state: { state, text }, state one of worker (a
  // service worker was found: first, since nothing on the page can be
  // trusted then), link (a client's link down: nothing else on the line is
  // current then), checking, error, stale, fresh, or '' (nothing to say: no
  // ready account has synced).
  function lineState() {
    var worker = lk && lk.workerLine(workers);
    if (worker) return worker;
    var down = lk && lk.lineState(linkInfo);
    if (down) return down;
    var accts = readyAccounts();
    if (askedAt || accts.some(busy)) return { state: 'checking', text: 'Checking…' };
    var sick = accts.filter(function (a) { return (a.failures || 0) >= SICK_FAILURES; });
    if (sick.length) return { state: 'error', text: sick[0].name + ': sync failing' + (sick.length > 1 ? ' +' + (sick.length - 1) : '') };
    // The view is only as fresh as its stalest account.
    var oldest = NaN;
    accts.forEach(function (a) {
      var t = a.lastSync ? Date.parse(a.lastSync) : NaN;
      if (!isNaN(t) && !(t >= oldest)) oldest = t;
    });
    if (isNaN(oldest)) return { state: '', text: '' };
    var age = Math.max(0, Date.now() - oldest);
    return { state: age > 3 * syncEvery ? 'stale' : 'fresh', text: 'Updated ' + ago(age) };
  }

  function renderLine() {
    if (!lineEl) return;
    var st = lineState();
    lineEl.dataset.state = st.state;
    lineText.textContent = st.text;
    // A client's version nudge, appended muted (link.js; none on a server).
    var nudge = lk ? lk.nudge(linkInfo) : '';
    if (lineNudge) lineNudge.textContent = st.text ? nudge : '';
    if (lineNudge && !st.text) lineText.textContent = nudge;
    lineEl.classList.toggle('nudged', !!nudge);
    lineEl.setAttribute('aria-label', (st.text || nudge || 'Sync') + (nudge && st.text ? ' · ' + nudge : '') + ' · details');
    if (st.state === 'checking' && !spinTimer) {
      lineSpin.textContent = SPIN_FRAMES[spinFrame];
      spinTimer = setInterval(function () {
        spinFrame = (spinFrame + 1) % SPIN_FRAMES.length;
        lineSpin.textContent = SPIN_FRAMES[spinFrame];
      }, 120);
    } else if (st.state !== 'checking' && spinTimer) {
      clearInterval(spinTimer);
      spinTimer = 0;
      lineSpin.textContent = '';
    }
    // The age ticks over only while one shows: no timer runs for nothing.
    clearTimeout(ageTimer);
    ageTimer = st.state === 'fresh' || st.state === 'stale' ? setTimeout(renderLine, AGE_TICK) : 0;
    // Announce a change of state, not the age ticking over.
    var said = st.state + (st.state === 'error' || st.state === 'link' || st.state === 'worker' ? st.text : '');
    if (lineLive && lineSaid !== null && said !== lineSaid) lineLive.textContent = st.text;
    lineSaid = said;
    if (syncInfo && syncInfo.open) syncInfo.replaceChildren(syncDetails());
    var inHelp = help && help.open && help.querySelector('.syncinfo');
    if (inHelp) inHelp.replaceWith(syncDetails());
  }

  // lineAsk: a sync was just asked for; Checking… until lineAsked.
  function lineAsk() {
    askedAt = Date.now();
    clearTimeout(askTimer);
    askTimer = setTimeout(lineAsked, ASK_WAIT);
    renderLine();
  }

  function lineAsked() {
    if (!askedAt) return;
    askedAt = 0;
    clearTimeout(askTimer);
    renderLine();
  }

  // lineNews takes an account's new view (SSE `account`) or a sync's start
  // or end. News of a queued or running sync answers a request.
  function lineNews(a) {
    if (a.queued || a.running) {
      busySince[a.name] = Date.now();
      askedAt = 0;
      clearTimeout(askTimer);
    }
    renderLine();
  }

  // syncEvent applies `syncing` (running) or a sync's `sync` end to the
  // account's copy ahead of the `account` view that follows each.
  function syncEvent(name, running) {
    var a = name && accounts[name];
    if (!a) return;
    a.running = running;
    if (running) a.queued = false;
    lineNews(a);
  }

  function clock(t) {
    var d = new Date(t);
    var today = new Date().toDateString() === d.toDateString();
    return (today ? '' : d.toLocaleDateString(undefined, { day: 'numeric', month: 'short' }) + ' ') +
      d.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
  }

  // syncDetails is the details: each account's last sync, what it's doing,
  // and its failures, for dialog#syncinfo and the top of the ? overlay.
  function syncDetails() {
    var box = el('div', 'syncinfo');
    box.appendChild(el('h2', null, 'Sync'));
    var dl = el('dl');
    acctOrder.forEach(function (n) {
      var a = accounts[n];
      var dd = el('dd');
      var t = a.lastSync ? Date.parse(a.lastSync) : NaN;
      dd.appendChild(document.createTextNode(isNaN(t) ? 'never synced' : 'synced ' + clock(t) + ' '));
      if (!isNaN(t)) dd.appendChild(el('span', 'when', '(' + ago(Date.now() - t) + ')'));
      var doing = a.state !== 'ready' ? ({ pulling: 'first download', 'needs-pull': 'waiting to download', reauth: 'needs reconnecting',
        unconfigured: 'not set up', unauthorized: 'not connected' })[a.state] || a.state
        : busy(a) ? (a.running ? 'checking now' : 'check queued') : '';
      if (doing) dd.appendChild(document.createTextNode(' · ' + doing));
      if (a.failures > 0 || a.error) {
        dd.appendChild(el('br'));
        dd.appendChild(el('span', 'bad', (a.failures > 0 ? a.failures + ' failed sync' + (a.failures === 1 ? '' : 's') : 'error') +
          (a.error ? ': ' + a.error : '')));
      }
      dl.appendChild(el('dt', null, n));
      dl.appendChild(dd);
    });
    if (!acctOrder.length) dl.appendChild(el('dd', null, 'no accounts'));
    box.appendChild(dl);
    // A client's link: its state and both builds (none on a server).
    var rows = lk ? lk.details(linkInfo) : [];
    if (rows.length) {
      box.appendChild(el('h2', null, 'Link'));
      var ldl = el('dl');
      rows.forEach(function (row) {
        ldl.appendChild(el('dt', null, row[0]));
        ldl.appendChild(el('dd', null, row[1]));
      });
      box.appendChild(ldl);
    }
    var every = syncEvery < 60000 ? Math.round(syncEvery / 1000) + 's' : Math.round(syncEvery / 60000) + 'm';
    var foot = el('p', 'foot', 'Checks every ' + every + ' · ');
    foot.appendChild(el('kbd', null, 'R'));
    foot.appendChild(document.createTextNode(' checks now'));
    box.appendChild(foot);
    return box;
  }

  function showSyncInfo() {
    if (!syncInfo) {
      syncInfo = document.createElement('dialog');
      syncInfo.id = 'syncinfo';
      syncInfo.addEventListener('click', function () { syncInfo.close(); });
      document.body.appendChild(syncInfo);
    }
    syncInfo.replaceChildren(syncDetails());
    syncInfo.showModal();
  }

  if (lineEl) lineEl.addEventListener('click', showSyncInfo);
  renderLine();

  function openEvents() {
    if (events || !window.EventSource) return;
    // Reconnects are the browser's; each new stream opens with hello.
    events = new EventSource('/events');
    events.addEventListener('hello', whenReady(function (e) {
      var h = null;
      try { h = JSON.parse(e.data); } catch (err) { return; }
      if (h) onHello(h);
    }));
    events.addEventListener('view', whenReady(function (e) {
      var ev = null;
      try { ev = JSON.parse(e.data); } catch (err) { return; }
      if (ev && typeof ev.epoch === 'string' && typeof ev.gen === 'number') onView(ev);
    }));
    events.addEventListener('syncing', function (e) {
      var d = null;
      try { d = JSON.parse(e.data); } catch (err) { return; }
      syncEvent(d.account, true);
    });
    // A sync's end, for the status line; one that pulled something is
    // also a `view`, which refreshes.
    events.addEventListener('sync', function (e) {
      var d = null;
      try { d = JSON.parse(e.data); } catch (err) { return; }
      if (d && d.op !== 'push') syncEvent(d.account, false);
    });
    events.addEventListener('theme', reloadTheme);
    // A client daemon's own: its link to the server changed.
    events.addEventListener('link', function (e) {
      try { setLink(JSON.parse(e.data)); } catch (err) { /* ignore */ }
    });
    events.addEventListener('account', function (e) {
      try { accountEvent(JSON.parse(e.data)); } catch (err) { /* ignore */ }
    });
    events.addEventListener('auth', function (e) {
      var d = null;
      try { d = JSON.parse(e.data); } catch (err) { return; }
      if (d.ok) flash(d.account + ' reconnected to Gmail');
      else flash('Reconnecting ' + d.account + ' failed: ' + (d.error || 'cancelled'), 'error');
    });
  }

  // A list restored from the back-forward cache after a triage elsewhere
  // (archive on the thread page, read-on-open) is stale: reload it.
  var genAtHide = null;
  window.addEventListener('pagehide', function () { genAtHide = gen(); });
  window.addEventListener('pageshow', function (e) {
    if (e.persisted && gen() !== genAtHide) {
      if (split && L.root) refreshList();
      else if (primary === 'list') { location.reload(); return; }
    }
    flashStored();
  });

  setPrimary(primary); // also shows the footer's keys
  if (primary === 'list') storage(function (s) { s.setItem(VIEW_KEY, L.url); });
  if (L.root) { L.label = pageLabel(document); initList(L.root); }
  if (T.root) {
    T.label = pageLabel(document);
    T.url = location.pathname;
    initThread(T.root);
    readOnOpen();
  }
  ready.then(initSplit);
  openEvents(); // every page: sync refreshes lists, theme restyles any page
})();
