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

  function storage(fn) {
    try { return fn(window.sessionStorage); } catch (e) { return null; }
  }

  // ---- panes and cursors --------------------------------------------------
  // A page holds up to two panes inside #panes: L, the thread list, and T,
  // the open thread. Below the split width only the one the URL names
  // (`primary`) shows and the page works as separate navigations. At the
  // split width (initSplit) both show side by side, `focus` says which one
  // the keys drive, and either can be replaced by content fetched from its
  // own page (fetchMain). initList/initThread take the element to drive and
  // drop whatever they drove before.

  var L = { root: document.querySelector('main.list'), items: [], sel: -1, url: location.pathname + location.search, title: document.title };
  var T = { root: document.querySelector('main.thread'), items: [], sel: -1, url: null, pending: null, abort: null };
  var primary = T.root ? 'thread' : L.root ? 'list' : null;
  var focus = primary;
  var split = false;

  function cur(c) { return c.items[c.sel] || null; }

  function selKey() { return 'pneu:sel:' + L.url; }

  function select(c, i, scroll) {
    if (!c.items.length) return;
    i = Math.max(0, Math.min(c.items.length - 1, i));
    if (c.items[c.sel]) c.items[c.sel].classList.remove('selected');
    c.sel = i;
    var el = c.items[i];
    el.classList.add('selected');
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
      want = saved ? { thread: saved, index: slot } : { index: 0 };
    }
    var start = tri ? tri.restoreIndex(L.items.map(rowKey), want) : 0;
    select(L, start, scroll !== undefined ? scroll : start > 0);
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
  // the first time, with the current theme.
  function paintMail(article, mail) {
    var built = Pneu.renderMailFrame(mail.html, mail.cids, origin, {
      frame: mail.frame || undefined, onKeydown: onKey, partBase: mail.partBase,
      remoteImages: mail.remoteImages, theme: frameTheme(article),
    });
    mail.frame = built.frame;
    mail.colors = built.colors;
    return built;
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
    if (!article.classList.contains('collapsed')) renderBody(article);
  }

  // initThread drives root: the page's own thread, or one fetched into the
  // right pane. The previous instance's body loads are aborted; its
  // listeners went with its element.
  function initThread(root) {
    if (T.abort) T.abort.abort();
    T.abort = window.AbortController ? new AbortController() : null;
    T.root = root;
    T.items = Array.prototype.slice.call(root.querySelectorAll('article.message'));
    T.sel = -1;
    leaving = false;
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
      return { main: document.adoptNode(main), title: doc.title };
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
    fetchMain(url, 'thread').then(function (got) {
      if (seq !== showSeq) return;
      T.pending = null;
      if (!split) { leaving = false; return; } // went narrow meanwhile: the page stays what its URL says
      if (T.root) T.root.replaceWith(got.main);
      else panes.appendChild(got.main);
      T.url = url;
      initThread(got.main);
      var id = rowId(got.main.dataset.account, got.main.dataset.thread);
      if (noRead[id]) delete noRead[id];
      else readOnOpen();
      if (got.title) document.title = got.title;
      after();
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
    showSeq++;
    T.pending = null;
    leaving = false;
    if (T.abort) T.abort.abort();
    if (T.root) T.root.remove();
    T.root = null;
    T.items = [];
    T.sel = -1;
    T.url = null;
    setFocus('list');
    setURL(L.url, false);
    document.title = L.title || 'Pneu';
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
      if (got.title) L.title = got.title;
      markNav(url);
      initList(got.main, want, false);
      got.main.scrollTop = top;
      var row = cur(L);
      if (row) row.scrollIntoView({ block: 'nearest' });
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

  function tagError(res, data) {
    var err = new Error((data && data.error) || 'HTTP ' + res.status);
    err.status = res.status;
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
          headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' },
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
      return data;
    }).finally(function () {
      inflight--;
      lastTag = Date.now();
      storage(function (s) { s.setItem(LAST_TAG_KEY, String(lastTag)); });
    });
  }

  function gen() { return storage(function (s) { return s.getItem(GEN_KEY); }); }

  function loadStack() { return tri.parse(storage(function (s) { return s.getItem(UNDO_KEY); })); }
  function saveStack(st) { storage(function (s) { s.setItem(UNDO_KEY, JSON.stringify(st)); }); }

  // remember records an action the server can undo (resp.id is absent when
  // nothing was written, e.g. unstar with nothing flagged).
  function remember(resp, account, thread, prev) {
    if (!resp.id) return false;
    saveStack(tri.push(loadStack(), {
      id: resp.id, action: resp.action, account: account, thread: thread, prev: prev, at: Date.now(),
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

  function done(action, undoable) {
    flash(tri.label(action) + (undoable ? ' · z to undo' : ''), '', undoable ? { label: 'Undo', run: undo } : null);
  }

  function fail(what, err) {
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

  // listCount keeps the list's title row count (list.html) with the rows.
  function listCount() {
    var n = L.root && L.root.querySelector('.pane-title .n');
    if (n) n.textContent = String(L.items.length);
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

  // Archive and trash act on the whole thread (data-thread-ids), as on the
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

  // Past the server's id cap archive/trash send the thread id alone.
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

  // threadRemove archives or trashes the open thread. In the split it is the
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
      ['j / k', 'Next / previous row (list) · scroll 3 lines (thread)'],
      ['n / p', 'Next / previous message (thread)'],
      ['Space, ⇧Space', 'Page down / up (thread)'],
      ['Enter, o', 'Open thread (list) · fold message (thread)'],
      ['u, Esc', 'Back to the list (Esc first leaves a field) · Esc on the list closes the thread'],
    ]],
    ['Panes', [
      ['h', 'Keys to the list'],
      ['l', 'Keys to the thread (opens the cursor row)'],
      ['Tab, ⇧Tab', 'Switch pane (not inside a mail body)'],
      ['+', 'Maximize the active pane · + again restores'],
    ]],
    ['Act', [
      ['e', 'Archive'],
      ['t, #', 'Trash'],
      ['s', 'Star / unstar'],
      ['U', 'Mark unread'],
      ['z', 'Undo'],
      ['R', 'Sync now'],
      ['r', 'Reply to sender (thread)'],
      ['a', 'Reply all (thread)'],
      ['w, c', 'Write'],
      ['v', 'Open in Gmail (new tab)'],
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
    if (help.open) help.close();
    else help.showModal();
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

  // R: POST /sync queues a sync on every account; the SSE `sync` event
  // re-renders the list if the pull brought anything.
  function syncNow() {
    flash('Syncing…');
    fetch('/sync', { method: 'POST', credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (res) {
        return res.json().catch(function () { return null; }).then(function (data) {
          if (!res.ok || !data || !data.ok) throw new Error((data && data.error) || 'HTTP ' + res.status);
        });
      })
      .catch(function (err) { fail('Sync', err); });
  }

  var keys = (Pneu.keys = {
    list: {
      j: move(L, 1),
      k: move(L, -1),
      Escape: closePane,
      Enter: openRow,
      o: openRow,
      e: whenReady(function () { listRemove('archive'); }),
      '#': whenReady(function () { listRemove('trash'); }),
      t: whenReady(function () { listRemove('trash'); }),
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
      s: whenReady(threadStar),
      U: whenReady(threadUnread),
      r: reply(false),
      a: reply(true),
      v: openGmail,
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

  Pneu.selection = function () {
    var k = activeKind();
    return k === 'list' ? cur(L) : k === 'thread' ? cur(T) : null;
  };

  function onKey(e) {
    lastKey = Date.now();
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
    var kind = activeKind();
    var map = kind ? keys[kind] : null;
    var fn = map && own.call(map, e.key) && map[e.key];
    if (typeof fn !== 'function' && (kind || own.call(ANYWHERE, e.key))) fn = own.call(keys.global, e.key) && keys.global[e.key];
    if (typeof fn !== 'function') return;
    // Enter on a focused link or button keeps its native meaning.
    if (e.key === 'Enter' && t && t.nodeType === 1 && t.closest('a, button')) return;
    e.preventDefault();
    fn(e);
  }

  document.addEventListener('keydown', onKey);

  // ---- sync ---------------------------------------------------------------
  // A list page reloads when a sync lands, unless you were typing or a tag
  // request was in flight or answered in the last two seconds; then it waits
  // for a quiet moment. Selection and the status line survive the reload.
  // In the split the list pane re-renders in place instead (loadList): the
  // cursor stays on its thread, or its slot, and the open thread is left
  // alone. main.go broadcasts `syncing` when an account's sync starts and
  // `sync` when it ends, failures included (`changed` false), never for a
  // push; the op check below is belt and braces should that ever change,
  // since a push changes nothing local.

  // One connection per page, every page: `sync` refreshes a list (the others
  // ignore it), `theme` restyles whatever is showing.
  var events = null;
  var reloadTimer = 0;

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

  // ---- sync glyph ---------------------------------------------------------
  // The header's far-right glyph (SPEC "Sync state"): a braille spinner while
  // any account's sync runs, empty when idle. The per-account cap retires a
  // spin whose end event was lost across an SSE reconnect; a real sync is
  // bounded by gmi's own 10m timeout, so the cap sits just above it.

  var SPIN_FRAMES = '⣾⣽⣻⢿⡿⣟⣯⣷';
  var spinEl = document.getElementById('sync');
  var spinOn = {}; // account -> cap timer
  var spinTimer = 0;
  var spinFrame = 0;

  function spinDraw() {
    if (!spinEl) return;
    var any = Object.keys(spinOn).length > 0;
    if (any && !spinTimer) {
      spinEl.textContent = SPIN_FRAMES[spinFrame];
      spinTimer = setInterval(function () {
        spinFrame = (spinFrame + 1) % SPIN_FRAMES.length;
        spinEl.textContent = SPIN_FRAMES[spinFrame];
      }, 120);
    } else if (!any && spinTimer) {
      clearInterval(spinTimer);
      spinTimer = 0;
      spinEl.textContent = '';
    }
  }

  function spinStart(account) {
    account = account || '';
    clearTimeout(spinOn[account]);
    spinOn[account] = setTimeout(function () { spinStop(account); }, 11 * 60 * 1000);
    spinDraw();
  }

  function spinStop(account) {
    account = account || '';
    if (!(account in spinOn)) return; // an end whose start predates this page
    clearTimeout(spinOn[account]);
    delete spinOn[account];
    spinDraw();
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
      method: 'POST', credentials: 'same-origin', headers: { Accept: 'application/json' },
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
      fail('Reconnect ' + name, err);
    });
  }

  function acctLine(a) {
    var p = el('p');
    p.dataset.account = a.name;
    p.appendChild(el('span', 'name', a.name));
    p.appendChild(document.createTextNode(': '));
    var text = function (t, cls) { p.appendChild(el('span', cls, t)); };
    switch (a.state) {
      case 'unconfigured':
        text('not set up. Run ');
        p.appendChild(el('code', null, 'pneu account add ' + a.name + ' <address>'));
        break;
      case 'unauthorized':
        text('not connected to Gmail. Run ');
        p.appendChild(el('code', null, 'pneu account auth ' + a.name));
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
    // New mail lands newest first while a pull downloads: show it now and then.
    if (a.state === 'pulling' && a.progress && a.progress.phase === 'content' && Date.now() - pullRefreshAt > PULL_REFRESH) {
      pullRefreshAt = Date.now();
      reloadWhenQuiet();
    }
  }

  renderAccounts();

  function openEvents() {
    if (events || !window.EventSource) return;
    events = new EventSource('/events');
    events.addEventListener('syncing', function (e) {
      var d = null;
      try { d = JSON.parse(e.data); } catch (err) { /* spin unattributed */ }
      spinStart(d && d.account);
    });
    events.addEventListener('sync', function (e) {
      var d = null;
      try { d = JSON.parse(e.data); } catch (err) { /* reload anyway */ }
      if (d && d.op === 'push') return;
      spinStop(d && d.account);
      if (d && d.changed === false) return; // ended, but pulled nothing (or failed)
      reloadWhenQuiet();
    });
    events.addEventListener('theme', reloadTheme);
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
  if (L.root) initList(L.root);
  if (T.root) {
    T.url = location.pathname;
    initThread(T.root);
    readOnOpen();
  }
  ready.then(initSplit);
  openEvents(); // every page: sync refreshes lists, theme restyles any page
})();
