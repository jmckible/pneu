// actions.js — message actions that act on what the sender wrote
// (docs/actions.md): the unsubscribe confirmation (X), and the shared
// pieces later actions (o, L) use: the browser-open check and the arming
// rule for confirming controls. The pure half also runs under node --test
// (web/actions.test.js); the DOM half only in the browser.
//
// The sender is the adversary. Every sender-supplied string reaches this
// document through textContent, each in its own <bdi>, with format and bidi
// controls drawn visibly, and capped. Nothing runs that the user didn't see
// in full and confirm.
(function (root) {
  'use strict';

  // ---- pure -----------------------------------------------------------------

  var CF = /[\p{Cf}\p{Cs}\u2028\u2029]/u;

  // shown reports whether code point ch is drawn as an escape: format and
  // bidi controls, lone surrogates, line/paragraph separators, and C0/C1
  // controls (a multiline field keeps its newlines and tabs).
  function shown(ch, multiline) {
    var c = ch.codePointAt(0);
    if (c < 0x20 || (c >= 0x7f && c <= 0x9f)) return !(multiline && (c === 0x0a || c === 0x09));
    return CF.test(ch);
  }

  function escapeCp(ch) {
    var h = ch.codePointAt(0).toString(16).toUpperCase();
    while (h.length < 4) h = '0' + h;
    return '\\u{' + h + '}';
  }

  // segments splits s into plain runs {t} and escaped controls {c}.
  function segments(s, multiline) {
    var out = [], run = '';
    for (var ch of String(s == null ? '' : s)) {
      if (shown(ch, multiline)) {
        if (run) { out.push({ t: run }); run = ''; }
        out.push({ c: escapeCp(ch) });
      } else {
        run += ch;
      }
    }
    if (run) out.push({ t: run });
    return out;
  }

  // visible is segments as one string, as the page draws it.
  function visible(s, multiline) {
    return segments(s, multiline).map(function (g) { return g.t != null ? g.t : g.c; }).join('');
  }

  // cap cuts s to n code points (before escaping), marking a cut with '…'.
  function cap(s, n) {
    var a = Array.from(String(s == null ? '' : s));
    if (a.length <= n) return { text: a.join(''), cut: false };
    return { text: a.slice(0, n).join('') + '…', cut: true };
  }

  // Caps in code points. Labels are cut with a visible '…'. Fields that say
  // what will be sent or opened (url, subject, body) are never cut: past
  // their cap the confirmation isn't offered, since a cut would hide part of
  // what runs.
  // The recipient is never cut either: the server allows at most 254
  // octets, and the dialog wraps it in full.
  var CAPS = { context: 200, account: 100, signedBy: 253, from: 320, destination: 300 };
  var WHOLE = { url: 4096, subject: 2048, body: 16384, to: 254 };

  var IPV4_LAST = /^(?:\d+|0x[0-9a-f]*)$/i;

  // browserCheck is the one check every browser-opened destination passes
  // (unsubscribe open-in-tab, o, link hints) at the moment of opening:
  // http(s) only, not the app's origin, not an IP literal, not localhost,
  // *.localhost or a single-label host. ok carries the normalized URL, which
  // is what is shown and what opens.
  function browserCheck(raw, appOrigins) {
    var u;
    try { u = new URL(String(raw)); } catch (e) { return { ok: false, reason: 'not a web address' }; }
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return { ok: false, reason: 'not a web address' };
    var origins = [].concat(appOrigins || []);
    for (var i = 0; i < origins.length; i++) {
      if (origins[i] && u.origin === origins[i]) return { ok: false, reason: 'points at pneu itself' };
    }
    var h = u.hostname.toLowerCase().replace(/\.+$/, '');
    if (!h) return { ok: false, reason: 'no host' };
    // The URL parser has already turned any numeric host (0x7f.1, 2130706433)
    // into dotted IPv4; a last label that is a number is one regardless.
    if (h.charAt(0) === '[' || IPV4_LAST.test(h.split('.').pop())) return { ok: false, reason: 'an IP address' };
    if (h === 'localhost' || /\.localhost$/.test(h)) return { ok: false, reason: 'a local address' };
    if (h.indexOf('.') < 0) return { ok: false, reason: 'a single-label host' };
    return { ok: true, url: u.href };
  }

  // destination is what the one-click line shows, derived from the URL that
  // will be POSTed: scheme and host, the port only when it isn't 443 (URL
  // drops default ports), punycode as the parser leaves it.
  function destination(raw) {
    try {
      var u = new URL(String(raw));
      if (u.protocol !== 'https:' && u.protocol !== 'http:') return null;
      return u.protocol + '//' + u.host;
    } catch (e) { return null; }
  }

  // Arming: a confirming control acts only once its dialog has rendered and
  // the key that brought it up has been released. Until then every key and
  // click is dropped; repeats always are. init is that key's event ({code,
  // key}), or null when a click brought it up.
  function Arming(init) {
    this.code = (init && init.code) || '';
    this.key = (init && init.key) || '';
    this.released = !init;
    this.drawn = false;
  }
  Arming.prototype.rendered = function () { this.drawn = true; };
  Arming.prototype.keyup = function (e) {
    if (this.released || !e) return;
    if (this.code ? e.code === this.code : String(e.key).toLowerCase() === this.key.toLowerCase()) this.released = true;
  };
  Arming.prototype.armed = function () { return this.drawn && this.released; };
  Arming.prototype.accept = function (e) { return !(e && e.repeat) && this.armed(); };

  // Keys that inspect the armed dialog natively: Tab moves between the
  // disclosure and the scrollable fields; the rest scroll the focused one.
  var SCROLL = { ArrowUp: 1, ArrowDown: 1, ArrowLeft: 1, ArrowRight: 1, PageUp: 1, PageDown: 1, Home: 1, End: 1, ' ': 1 };

  // dialogKey decides a keydown inside the dialog (which never lets one
  // reach the app's map). armed: the dialog's arming (Arming.armed());
  // keys: the phase's key map; target: 'button', 'summary' or 'other', what
  // has focus. Returns
  //   'act'    run keys[e.key]
  //   'press'  Enter/Space on a focused button: run that button's action
  //   'native' leave the browser's default (focus, toggle, scroll)
  //   'drop'   nothing
  // Nothing but 'drop' before arming; acting never on a repeat.
  function dialogKey(e, armed, keys, target) {
    if (!armed || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return 'drop';
    var k = e.key;
    if (keys && Object.prototype.hasOwnProperty.call(keys, k) && typeof keys[k] === 'function') return e.repeat ? 'drop' : 'act';
    if (k === 'Tab') return 'native'; // from any focus, a button's included
    var press = k === 'Enter' || k === ' ';
    if (target === 'button') return press && !e.repeat ? 'press' : 'drop';
    if (target === 'summary' && press) return e.repeat ? 'drop' : 'native';
    if (SCROLL[k] === 1) return 'native';
    return 'drop';
  }

  // previewURL is the GET for msgid (as data-msgid carries it: already
  // url.PathEscape'd by the server), and the option after index.
  function previewURL(account, msgid, after) {
    return '/unsubscribe/' + encodeURIComponent(account) + '/' + msgid +
      (after != null ? '?after=' + encodeURIComponent(after) : '');
  }

  var A = {
    segments: segments, visible: visible, cap: cap, CAPS: CAPS, WHOLE: WHOLE,
    browserCheck: browserCheck, destination: destination, Arming: Arming, previewURL: previewURL,
    dialogKey: dialogKey,
  };
  if (typeof module === 'object' && module.exports) {
    module.exports = A;
    return;
  }

  var Pneu = (root.Pneu = root.Pneu || {});
  Pneu.actions = A;

  // ---- DOM ------------------------------------------------------------------

  function appOrigins() {
    var o = [location.origin];
    if (document.body && document.body.dataset.origin) o.push(document.body.dataset.origin);
    return o;
  }

  // openInBrowser opens url in the default browser's tab if browserCheck
  // passes; returns the check.
  function openInBrowser(url) {
    var c = browserCheck(url, appOrigins());
    if (c.ok) window.open(c.url, '_blank', 'noopener,noreferrer');
    return c;
  }
  A.openInBrowser = openInBrowser;

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  // field is one untrusted string: its own isolated <bdi>, controls drawn as
  // escapes. n caps a label; whole (a limit) refuses rather than cuts.
  function field(s, opts) {
    opts = opts || {};
    var b = el('bdi', 'untrusted' + (opts.cls ? ' ' + opts.cls : ''));
    var text = String(s == null ? '' : s);
    if (opts.cap) text = cap(text, opts.cap).text;
    segments(text, opts.multiline).forEach(function (g) {
      if (g.t != null) b.appendChild(document.createTextNode(g.t));
      else b.appendChild(el('span', 'ctl', g.c));
    });
    return b;
  }

  function tooLong(s, limit) { return Array.from(String(s == null ? '' : s)).length > limit; }

  // ---- the session ------------------------------------------------------
  // One unsubscribe at a time, bound to the message identity taken when X
  // was pressed ({account, msgid, article, root}); ctx (app.js):
  //   flash(text, kind)  the status line
  //   archive(ident)     archive ident's thread (the thread pane's e)
  //   onClose()          the dialog has closed

  var S = null;

  function unsubscribe(ident, e, ctx) {
    if (S || !ident || (e && e.repeat)) return;
    var s = S = { ident: ident, ctx: ctx, arm: new Arming(e ? { code: e.code, key: e.key } : null), dlg: null, after: null };
    // Leaving the window before arming (the keyup may never come here):
    // nothing has been confirmed, so the session just goes.
    s.onBlur = function () { if (live(s) && !s.arm.armed()) cancel(); };
    window.addEventListener('blur', s.onBlur);
    preview(null);
  }

  // keyup feeds every keyup (the app document and mail frames) to the
  // current arming.
  function keyup(e) { if (S && S.arm) S.arm.keyup(e); }

  // pendingPreview: X was pressed and the preview hasn't answered yet.
  function pendingPreview() { return !!(S && !S.dlg); }

  // pendingKey takes every keydown while the preview loads, before the app's
  // key map (app.js asks first), so nothing falls through to archive or
  // reply: Esc cancels, the rest are dropped. Returns whether it took e.
  function pendingKey(e) {
    if (!pendingPreview()) return false;
    e.preventDefault();
    if (e.stopPropagation) e.stopPropagation();
    if (e.key === 'Escape' && !e.repeat) cancel();
    return true;
  }

  // cancel drops the session: a preview in flight is aborted and the dialog
  // closes. An unsubscribe already POSTed runs on; its outcome goes to the
  // status line.
  function cancel() {
    if (!S) return;
    var s = S;
    if (s.abort) s.abort.abort();
    finish(s);
  }

  // finish ends session s: the dialog closes and goes, synchronously (the
  // close event comes a task later, and a new X may come first).
  function finish(s) {
    if (live(s)) S = null;
    if (s.finished) return;
    s.finished = true;
    window.removeEventListener('blur', s.onBlur);
    if (!s.dlg) return;
    if (s.dlg.open) s.dlg.close();
    s.dlg.remove();
    if (s.ctx.onClose) s.ctx.onClose();
  }

  // cancelFor cancels a session bound to article (its frame was replaced).
  function cancelFor(article) { if (S && S.ident.article === article) cancel(); }

  A.unsubscribe = unsubscribe;
  A.keyup = keyup;
  A.cancel = cancel;
  A.cancelFor = cancelFor;
  A.pendingPreview = pendingPreview;
  A.pendingKey = pendingKey;
  A.active = function () { return !!S; };

  function live(s) { return s === S; }

  function preview(after) {
    var s = S;
    s.after = after;
    if (s.abort) s.abort.abort();
    s.abort = new AbortController();
    fetch(previewURL(s.ident.account, s.ident.msgid, after), {
      credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' }, signal: s.abort.signal,
    }).then(function (res) {
      return res.json().catch(function () { return null; }).then(function (data) {
        if (!res.ok || !data || typeof data.method !== 'string') {
          throw new Error((data && (data.reason || data.error)) || 'HTTP ' + res.status);
        }
        return data;
      });
    }).then(function (data) {
      if (!live(s)) return;
      s.abort = null;
      showPreview(data);
    }).catch(function (err) {
      if (!live(s) || (s.abort && s.abort.signal.aborted)) return;
      s.abort = null;
      var ctx = s.ctx;
      cancel();
      ctx.flash('Unsubscribe failed: ' + (err && err.message || 'network error'), 'error');
    });
  }

  // ---- the dialog -------------------------------------------------------
  // All keys stop here (nothing reaches the thread's map); dialogKey decides
  // the rest, and before arming it drops everything. Once armed, Tab, the
  // disclosure's Enter/Space and the scrolling keys work natively on the
  // disclosure and the scrollable fields (tabindex 0). Buttons stay out of
  // the tab order; one focused by a click acts on Enter/Space only through
  // dialogKey, so only armed, and a click counts only when the pointer went
  // down while armed, so a key-made click never does.

  function ensureDialog() {
    var s = S;
    if (s.dlg) return s.dlg;
    var d = el('dialog');
    d.id = 'unsub';
    d.tabIndex = -1;
    d.setAttribute('aria-label', 'Unsubscribe');
    d.addEventListener('keydown', function (e) {
      e.stopPropagation();
      s.downArmed = false; // a click this key makes is not a pointer's
      var t = e.target && e.target.closest ? e.target : null;
      var btn = t && t.closest('button');
      var target = btn ? 'button' : t && t.closest('summary') ? 'summary' : 'other';
      var what = live(s) ? dialogKey(e, s.arm.armed(), s.keys, target) : 'drop';
      if (what === 'native') return;
      e.preventDefault();
      if (what === 'act') s.keys[e.key](e);
      else if (what === 'press' && typeof btn.pneuAct === 'function') btn.pneuAct(e);
    });
    d.addEventListener('keyup', function (e) {
      e.stopPropagation();
      // Space activates a button on keyup: never here.
      if ((e.key === ' ' || e.key === 'Enter') && e.target && e.target.closest && e.target.closest('button')) e.preventDefault();
      keyup(e);
    });
    // Esc's close request: the keydown handler decides.
    d.addEventListener('cancel', function (e) { e.preventDefault(); });
    d.addEventListener('pointerdown', function () { s.downArmed = live(s) && s.arm.armed(); }, true);
    d.addEventListener('click', function (e) {
      if (e.target === d && s.downArmed) close(); // the backdrop
    });
    d.addEventListener('close', function () { finish(s); });
    s.dlg = d;
    document.body.appendChild(d);
    return d;
  }

  function close() { cancel(); }

  function button(key, label, fn) {
    var b = el('button');
    b.type = 'button';
    b.tabIndex = -1;
    b.appendChild(el('kbd', '', key));
    b.appendChild(document.createTextNode(' ' + label));
    var s = S;
    b.addEventListener('click', function (e) {
      e.stopPropagation();
      var ok = live(s) && s.downArmed && s.arm.armed();
      s.downArmed = false;
      if (ok) fn(null);
    });
    // Enter/Space on the focused button, as dialogKey allows it (armed).
    b.pneuAct = function (e) { if (live(s) && s.arm.armed()) fn(e); };
    return b;
  }

  // rearm starts a new arming for the key event (or null: a click, or an
  // answer arriving) that moved the dialog on. A key the previous arming was
  // still waiting on stays required.
  function rearm(s, trigger) {
    var prev = s.arm;
    var a = new Arming(trigger ? { code: trigger.code, key: trigger.key } : null);
    if (!trigger && prev && !prev.released) { a.code = prev.code; a.key = prev.key; a.released = false; }
    s.arm = a;
    return a;
  }

  // phase replaces the dialog's content. parts: {title, body: [nodes],
  // actions: [[key, label, fn, keys?]]}. trigger is what moved here (see
  // rearm); undefined keeps the current arming (set when the move began).
  // The new content arms once drawn and that key is up.
  function phase(parts, trigger) {
    var s = S;
    var d = ensureDialog();
    if (trigger !== undefined) rearm(s, trigger);
    s.downArmed = false;
    var kids = [el('h2', null, parts.title)];
    (parts.body || []).forEach(function (n) { kids.push(n); });
    var bar = el('div', 'actions');
    var keys = {};
    (parts.actions || []).forEach(function (a) {
      bar.appendChild(button(a[0], a[1], a[2]));
      (a[3] || [a[0]]).forEach(function (k) { keys[k] = a[2]; });
    });
    kids.push(bar);
    s.keys = keys;
    d.replaceChildren.apply(d, kids);
    if (!d.open) d.showModal();
    d.focus({ preventScroll: true });
    var arm = s.arm;
    // Rendered: two frames on, the content has been laid out and painted.
    requestAnimationFrame(function () { requestAnimationFrame(function () { arm.rendered(); }); });
  }

  var CLOSE = ['Esc', 'Close', function () { close(); }, ['Escape', 'n', 'q']];
  var CANCEL = ['n', 'Cancel', function () { close(); }, ['n', 'Escape']];

  function row(dl, label, node) {
    dl.appendChild(el('dt', null, label));
    var dd = el('dd');
    dd.appendChild(node);
    dl.appendChild(dd);
    return dd;
  }

  function note(text, cls) { return el('p', 'note' + (cls ? ' ' + cls : ''), text); }

  // inspectable puts the disclosure, or a field that may scroll, in the tab
  // order, for the armed dialog's native keys.
  function inspectable(node) {
    node.tabIndex = 0;
    return node;
  }

  // destLine is the destination on its own line above everything else, in
  // space reserved for it, so no label above or beside can push it out.
  function destLine(label, value, cls) {
    var p = el('p', 'dest');
    p.appendChild(el('span', 'label', label));
    var f = field(value, { cls: cls });
    if (/\burl\b/.test(cls || '')) inspectable(f);
    p.appendChild(f);
    return p;
  }

  function showPreview(data) {
    var s = S;
    var trig; // the arming set when this preview was asked for (X, or y)
    if (data.method === 'none') {
      phase({
        title: 'Nothing to unsubscribe',
        body: [note('This message offers no unsubscribe in its headers.'), reasonLine(data.reason)],
        actions: [CLOSE],
      }, trig);
      return;
    }
    var body = [];
    var dl = el('dl');
    var confirm = null, blocked = null;
    var title;
    if (data.method === 'one-click') {
      title = 'One-click unsubscribe';
      var dest = destination(data.url);
      if (!dest || tooLong(data.url, WHOLE.url)) blocked = 'The unsubscribe address can’t be shown in full.';
      body.push(destLine('POST to', dest ? cap(dest, CAPS.destination).text : '(unreadable address)', 'destination'));
      row(dl, 'signed by', field('d=' + (data.signedBy || ''), { cap: CAPS.signedBy }));
      var det = el('details');
      det.appendChild(inspectable(el('summary', null, 'Full URL'))); // Tab, then Enter/Space toggles
      det.appendChild(inspectable(field(data.url, { cls: 'url' })));
      body.push(dl, det);
      confirm = ['y', 'Unsubscribe', execute];
    } else if (data.method === 'mailto') {
      title = 'Unsubscribe by email';
      if (tooLong(data.subject, WHOLE.subject) || tooLong(data.body, WHOLE.body)) blocked = 'The message can’t be shown in full.';
      // The recipient in full, wrapped; past the server's own limit it
      // can't be, and nothing is offered.
      if (tooLong(data.to, WHOLE.to)) blocked = 'The recipient can’t be shown in full.';
      body.push(destLine('To', tooLong(data.to, WHOLE.to) ? cap(data.to, WHOLE.to).text : data.to, 'destination'));
      var from = field(data.from, { cap: CAPS.from });
      var acct = el('span');
      acct.appendChild(from);
      acct.appendChild(document.createTextNode(' · account '));
      acct.appendChild(field(data.account, { cap: CAPS.account }));
      row(dl, 'From', acct);
      inspectable(row(dl, 'Subject', field(data.subject)));
      body.push(dl);
      body.push(el('p', 'label', 'The complete message:'));
      var pre = inspectable(el('pre', 'mailbody'));
      if (data.body) pre.appendChild(field(data.body, { multiline: true }));
      else pre.appendChild(el('span', 'empty', '(empty body)'));
      body.push(pre);
      confirm = ['y', 'Send', execute];
    } else if (data.method === 'open') {
      title = 'Open the unsubscribe page';
      var chk = browserCheck(data.url, appOrigins());
      if (!chk.ok) blocked = 'This address was refused for safety: ' + chk.reason + '.';
      else if (tooLong(chk.url, WHOLE.url)) blocked = 'The address can’t be shown in full.';
      body.push(destLine('Opens', chk.ok ? chk.url : data.url, 'destination url'));
      body.push(note('opens in your browser'));
      confirm = ['y', 'Open', openPage];
    } else {
      phase({ title: 'Unsubscribe', body: [note('pneu doesn’t know this unsubscribe method.')], actions: [CLOSE] }, trig);
      return;
    }
    if (data.context) {
      var cdl = el('dl', 'context');
      row(cdl, 'Context', field(data.context, { cap: CAPS.context }));
      body.push(cdl);
    }
    if (!blocked && data.method !== 'open' && !data.token) {
      blocked = data.method === 'mailto'
        ? 'This account can’t send until its first download finishes.'
        : 'No confirmation token came with this preview.';
    }
    s.preview = data;
    if (blocked) {
      body.push(note(blocked, 'bad'));
      phase({ title: title, body: body, actions: [CLOSE] }, trig);
      return;
    }
    phase({ title: title, body: body, actions: [confirm, CANCEL] }, trig);
  }

  function reasonLine(reason) {
    var p = el('p', 'note');
    if (reason) p.appendChild(field(reason, { cap: 300 }));
    return p;
  }

  // ---- execute ----------------------------------------------------------

  function openPage(e) {
    var s = S;
    var c = openInBrowser(s.preview.url);
    if (!c.ok) {
      phase({ title: 'Refused', body: [note('This address was refused for safety: ' + c.reason + '. Nothing was opened.', 'bad')], actions: [CLOSE] }, e);
      return;
    }
    succeeded(e, 'Opened the unsubscribe page in your browser.');
  }

  function execute(e) {
    var s = S;
    var token = s.preview.token;
    // One POST per token: the phase below has no keys until it answers.
    phase({ title: 'Unsubscribing…', body: [note('Waiting for the answer.')], actions: [] }, e);
    fetch('/unsubscribe', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' },
      body: new URLSearchParams({ token: token }).toString(),
    }).then(function (res) {
      return res.json().catch(function () { return null; }).then(function (data) {
        if (res.status === 409 && data && data.state === 'expired') return data;
        if (!res.ok || !data || typeof data.state !== 'string') return null; // no answer we can read
        return data;
      });
    }, function () { return null; }).then(function (data) {
      if (data) outcome(s, data);
      else unknown(s, token, 0);
    });
  }

  // Delays before each ask for the stored result: two for a lost answer,
  // then every 3s while the server says the action is still running (a send
  // can outlast the POST's answer), for a minute at most.
  var POLL = [1500, 5000];
  var PENDING = { every: 3000, max: 20 };

  // unknown: the POST's answer was lost. It may have run; it is never sent
  // again. The stored result is asked for instead.
  function unknown(s, token, n, pending) {
    pending = pending || 0;
    if (n === 0 && !pending && live(s)) {
      phase({ title: 'Outcome unknown', body: [note('The answer was lost; checking whether it went through…')], actions: [CLOSE] }, null);
    }
    if (n >= POLL.length || pending > PENDING.max) {
      report(s, 'Unsubscribe outcome unknown. It may have gone through; it will not be retried.', 'error',
        { title: 'Outcome unknown', text: 'It may have gone through. pneu won’t send it again.' });
      return;
    }
    setTimeout(function () {
      fetch('/unsubscribe-result/' + encodeURIComponent(token), {
        credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' },
      }).then(function (res) {
        if (!res.ok) return null;
        return res.json().catch(function () { return null; });
      }, function () { return null; }).then(function (data) {
        var state = data && typeof data.state === 'string' ? data.state : '';
        if (state === 'pending') unknown(s, token, n, pending + 1);
        else if (state && state !== 'expired') outcome(s, data);
        else unknown(s, token, n + 1, pending);
      });
    }, pending ? PENDING.every : POLL[n]);
  }

  // report shows an end state in the dialog if it's still up, else on the
  // status line.
  function report(s, flashText, kind, dialog) {
    if (live(s)) phase({ title: dialog.title, body: [note(dialog.text, kind === 'error' ? 'bad' : '')], actions: [CLOSE] }, null);
    else s.ctx.flash(flashText, kind);
  }

  function outcome(s, data) {
    var method = s.preview.method;
    switch (data.state) {
      case 'ok':
        var what = method === 'mailto' ? 'Unsubscribe email sent.' : 'Unsubscribed.';
        if (live(s)) succeeded(null, what);
        else s.ctx.flash(what);
        return;
      case 'refused':
        report(s, 'Unsubscribe refused for safety; nothing was sent.', 'error',
          { title: 'Refused', text: 'The destination was refused for safety. Nothing was sent.' });
        return;
      case 'expired':
        if (!live(s)) { s.ctx.flash('Unsubscribe expired; nothing was sent.', 'error'); return; }
        phase({
          title: 'Expired',
          body: [note('The confirmation expired before it ran. Nothing was sent.')],
          actions: [['y', 'Preview again', function (e) { rearm(s, e); preview(s.after); }], CLOSE],
        }, null);
        return;
      case 'failed':
        var cat = data.category ? ' (' + data.category + ')' : '';
        if (!live(s)) { s.ctx.flash('Unsubscribe failed' + cat, 'error'); return; }
        var acts = [CLOSE];
        var body = [note('The unsubscribe request failed' + cat + '.', 'bad')];
        if (data.fallback && typeof s.preview.index === 'number') {
          var idx = s.preview.index;
          body.push(note('The sender lists another way to unsubscribe.'));
          acts = [['y', 'Preview the next option', function (e) { rearm(s, e); preview(idx); }], CLOSE];
        }
        phase({ title: 'Failed', body: body, actions: acts }, null);
        return;
      default:
        report(s, 'Unsubscribe outcome unknown.', 'error', { title: 'Outcome unknown', text: 'The server gave an answer pneu doesn’t know.' });
    }
  }

  function succeeded(e, text) {
    var s = S;
    phase({
      title: 'Done',
      body: [note(text)],
      actions: [['e', 'Archive the thread', function () {
        var ident = s.ident, ctx = s.ctx;
        close();
        ctx.archive(ident);
      }], CLOSE],
    }, e);
  }
})(typeof window !== 'undefined' ? window : this);
