// compose.js — the browser half of reply and compose (internal/web/compose.go
// is the contract): the HTML-only quote, address autocomplete, Cc/Bcc
// reveal, localStorage drafts, Ctrl+Enter.
//
// The top half is pure (no DOM, no fetch) and runs under node --test
// (web/compose.test.js). The bottom half wires it to main.compose and only
// runs in a browser. Every string it puts into the page goes through
// textContent or .value.
(function (root) {
  'use strict';

  // ---- html → text ---------------------------------------------------------
  // htmlToText walks anything shaped like a DOM node — {nodeType, nodeName,
  // childNodes, getAttribute, textContent} — so the tests can hand-build one.

  var SKIP = /^(HEAD|STYLE|SCRIPT|TITLE|META|LINK|TEMPLATE|NOSCRIPT|SVG|MATH|IFRAME|OBJECT|SELECT|DATALIST)$/;
  // Blocks separated by a blank line; everything in BLOCK1 by a line break.
  var BLOCK2 = /^(P|H[1-6]|UL|OL|DL|PRE|TABLE|FIGURE|HR|ADDRESS|FIELDSET)$/;
  var BLOCK1 = /^(DIV|SECTION|ARTICLE|HEADER|FOOTER|MAIN|NAV|ASIDE|CENTER|TR|TBODY|THEAD|TFOOT|CAPTION|DT|DD|FIGCAPTION|DETAILS|SUMMARY|FORM|BODY|HTML|LI)$/;
  var INVISIBLE = /[­͏​-‍⁠﻿]/g; // soft hyphen, zero-width padding in marketing preheaders

  function name(n) { return String(n.nodeName || '').toUpperCase(); }
  function attr(n, a) { return typeof n.getAttribute === 'function' ? n.getAttribute(a) : null; }

  function Out() { this.s = ''; this.marker = -1; }
  // block ensures the output ends in at least n newlines (none at the start,
  // none right after a list marker).
  Out.prototype.block = function (n) {
    if (!this.s || this.s.length === this.marker) return;
    var have = /\n*$/.exec(this.s)[0].length;
    while (have < n) { this.s += '\n'; have++; }
  };
  Out.prototype.br = function () { this.s += '\n'; };
  Out.prototype.text = function (t, pre) {
    t = String(t).replace(INVISIBLE, '');
    if (pre) { this.s += t.replace(/\r\n?/g, '\n'); return; }
    t = t.replace(/\s+/g, ' ');
    var s = this.s;
    if (!s || s.charAt(s.length - 1) === '\n' || s.charAt(s.length - 1) === ' ') t = t.replace(/^ /, '');
    this.s += t;
  };
  Out.prototype.raw = function (t) { this.s += t; };

  function walk(node, out, st) {
    if (node.nodeType === 3) { out.text(node.textContent == null ? node.nodeValue : node.textContent, st.pre); return; }
    if (node.nodeType !== 1 && node.nodeType !== 9 && node.nodeType !== 11) return;
    var nm = name(node);
    if (SKIP.test(nm)) return;
    // Hidden preheaders ("View in browser" teasers) aren't part of what was read.
    if (/display\s*:\s*none/i.test(attr(node, 'style') || '') || attr(node, 'hidden') != null) return;
    var kids = node.childNodes || [];
    var each = function (o, s) { for (var i = 0; i < kids.length; i++) walk(kids[i], o, s); };

    if (nm === 'BR') { out.br(); return; }
    if (nm === 'HR') { out.block(2); out.raw('---'); out.block(2); return; }
    if (nm === 'IMG') { var alt = attr(node, 'alt'); if (alt && alt.trim()) out.text(alt); return; }
    if (nm === 'BLOCKQUOTE') {
      var inner = new Out();
      each(inner, { pre: st.pre, depth: 0 });
      var q = finish(inner.s);
      if (!q) return;
      out.block(2);
      out.raw(quoteLines(q));
      out.block(2);
      return;
    }
    if (nm === 'LI') {
      out.block(1);
      out.raw(new Array(Math.max(0, st.depth - 1) + 1).join('  ') + '- ');
      out.marker = out.s.length;
      each(out, st);
      out.block(1);
      return;
    }
    if (nm === 'A') {
      var href = (attr(node, 'href') || '').trim();
      var start = out.s.length;
      each(out, st);
      var text = out.s.slice(start).trim();
      var bare = href.replace(/^mailto:/i, '');
      // An image link with no alt text adds nothing but a tracking URL; a
      // tel: link's text is the number (noted, it breaks Gmail's quote fold).
      if (text && href && href.charAt(0) !== '#' && !/^tel:/i.test(href) && text !== href && text !== bare) out.text(' <' + href + '>');
      return;
    }
    if (nm === 'TD' || nm === 'TH') { each(out, st); out.text(' '); return; }

    var b = BLOCK2.test(nm) ? 2 : BLOCK1.test(nm) ? 1 : 0;
    if ((nm === 'UL' || nm === 'OL') && st.depth > 0) b = 1; // nested list: no blank line
    if (b) out.block(b);
    var sub = st;
    if (nm === 'PRE') sub = { pre: true, depth: st.depth };
    else if (nm === 'UL' || nm === 'OL') sub = { pre: st.pre, depth: st.depth + 1 };
    each(out, sub);
    if (b) out.block(b);
  }

  // finish trims line ends, collapses blank runs to one, trims the ends.
  function finish(s) {
    var lines = s.split('\n').map(function (l) { return l.replace(/[ \t]+$/, ''); });
    var out = [];
    lines.forEach(function (l) {
      if (l === '' && (out.length === 0 || out[out.length - 1] === '')) return;
      out.push(l);
    });
    while (out.length && out[out.length - 1] === '') out.pop();
    return out.join('\n');
  }

  function htmlToText(node) {
    if (!node) return '';
    var out = new Out();
    walk(node, out, { pre: false, depth: 0 });
    return finish(out.s);
  }

  // quoteLines prefixes each line with "> " (">" for a blank one), as the
  // server's quoteText does. No trailing newline.
  function quoteLines(text) {
    return String(text).split('\n').map(function (l) { return l ? '> ' + l : '>'; }).join('\n');
  }

  // quote is the block appended after the attribution: one line each,
  // newline-terminated like the server's plain-text quote.
  function quote(text) {
    text = finish(String(text).replace(/\r\n?/g, '\n'));
    return text ? quoteLines(text) + '\n' : '';
  }

  // ---- recipient tokens -----------------------------------------------------

  // tokenAt finds the comma/semicolon-separated recipient around caret,
  // ignoring separators inside a quoted display name. start is past the
  // leading whitespace; query is what's typed from there to the caret.
  function tokenAt(value, caret) {
    value = String(value);
    caret = Math.max(0, Math.min(value.length, caret == null ? value.length : caret));
    var start = 0, end = value.length, inQ = false;
    for (var i = 0; i < value.length; i++) {
      var c = value.charAt(i);
      if (c === '\\' && inQ) { i++; continue; }
      if (c === '"') inQ = !inQ;
      else if (!inQ && (c === ',' || c === ';')) {
        if (i < caret) start = i + 1;
        else { end = i; break; }
      }
    }
    while (start < end && /\s/.test(value.charAt(start))) start++;
    var q = start <= caret ? value.slice(start, caret) : '';
    return { start: start, end: end, query: q.trim() };
  }

  // replaceToken swaps the token for the chosen address and a ", " so the
  // next one can be typed; returns the new value and caret.
  function replaceToken(value, tok, choice) {
    value = String(value);
    var before = value.slice(0, tok.start);
    if (before && !/\s$/.test(before)) before += ' ';
    var rest = value.slice(tok.end).replace(/^\s*[,;]?\s*/, '');
    var head = before + choice + ', ';
    return { value: head + rest, caret: head.length };
  }

  // ---- drafts ---------------------------------------------------------------

  var DRAFT_PREFIX = 'pneu:draft:';
  var DRAFT_CAP = 20;
  var FIELDS = ['to', 'cc', 'bcc', 'subject', 'body'];

  // A draft is {fields…, at, id, idAt, sent}. id is its draft id, the
  // form's message_id: the server's send log (compose.go, SendLog) answers
  // a repeat of it from its record instead of sending again, so it lives
  // with the draft and a reopened draft is the same send. idAt is when it
  // was minted or last submitted; sent, that it has been submitted.
  var ID_RE = /^<[0-9a-f]{32}@pneu\.[A-Za-z0-9.-]{1,253}>$/; // compose.go generatedIDRE
  // The server keeps a send's record 30 days (web.SendKeep); past that an
  // id guarantees nothing, and the draft gets a new one.
  var ID_KEEP = 30 * 24 * 60 * 60 * 1000;

  // parseDraft reads a stored draft, or null when it isn't one.
  function parseDraft(json) {
    var v;
    try { v = JSON.parse(json); } catch (e) { return null; }
    if (!v || typeof v !== 'object') return null;
    for (var i = 0; i < FIELDS.length; i++) if (typeof v[FIELDS[i]] !== 'string') return null;
    if (typeof v.at !== 'number') v.at = 0;
    if (typeof v.id !== 'string' || !ID_RE.test(v.id) || typeof v.idAt !== 'number') { delete v.id; delete v.idAt; }
    v.sent = v.sent === true;
    return v;
  }

  // draftID is the stored draft's id, or null when the page's fresh id
  // should stand. A submitted draft keeps its id whatever the clock says:
  // an unresolved send is abandoned only by an explicit discard. A draft
  // never submitted drops an id older than the server's record would be.
  function draftID(d, now) {
    if (!d || !d.id) return null;
    if (d.sent) return d.id;
    var age = now - d.idAt;
    return age >= 0 && age < ID_KEEP ? d.id : null;
  }

  // persistForSend stores draft (with its id) in local, reads it back, and
  // only then leaves the marker, with a second copy of the id, in session.
  // False when local storage didn't keep the id: the send must not go,
  // or a reopened draft would send again under a new id.
  function persistForSend(local, session, key, draft, now) {
    try {
      local.setItem(key, JSON.stringify(draft));
      var back = parseDraft(local.getItem(key));
      if (!back || back.id !== draft.id || !back.sent) return false;
    } catch (e) { return false; }
    try { session.setItem(SENDING_KEY, JSON.stringify({ key: key, id: draft.id, at: now })); } catch (e) { /* the second copy only */ }
    return true;
  }

  // capDrafts trims the stored drafts to DRAFT_CAP, oldest first, never
  // touching the active draft (key) or a submitted one: its id is what
  // keeps a resend from being a second send.
  function capDrafts(s, key) {
    var entries = [];
    for (var i = 0; i < s.length; i++) {
      var k = s.key(i);
      if (!k || k.indexOf(DRAFT_PREFIX) !== 0 || k === key) continue;
      var d = parseDraft(s.getItem(k));
      if (d && d.sent) continue;
      entries.push({ key: k, at: d ? d.at : 0 });
    }
    prune(entries, DRAFT_CAP - 1).forEach(function (k) { s.removeItem(k); });
  }

  // submitDraft is the submit handler's storage work: trim first, then
  // persist and read back, so nothing runs after the check that could
  // undo it. False: don't send.
  function submitDraft(local, session, key, draft, now) {
    try { capDrafts(local, key); } catch (e) { /* the persist below decides */ }
    return persistForSend(local, session, key, draft, now);
  }

  function sameFields(a, b) {
    return FIELDS.every(function (f) { return (a[f] || '') === (b[f] || ''); });
  }

  // prune takes [{key, at}] and returns the keys to delete so that at most
  // cap remain, newest by at kept.
  function prune(entries, cap) {
    cap = cap == null ? DRAFT_CAP : cap;
    var sorted = entries.slice().sort(function (a, b) { return (b.at || 0) - (a.at || 0); });
    return sorted.slice(cap).map(function (e) { return e.key; });
  }

  // ---- pending send ---------------------------------------------------------
  // Submitting keeps the draft (with its id) and leaves a marker in
  // sessionStorage, {key: the draft's storage key, id: a second copy, at}. Only the send's own
  // redirect (compose.go appends #sent to it) deletes the draft: leaving a
  // POST that never answered, by Back or a key, must not.

  var SENDING_KEY = 'pneu:sending';

  function parseMarker(json) {
    var v;
    try { v = JSON.parse(json); } catch (e) { return null; }
    if (!v || typeof v.key !== 'string' || v.key.indexOf(DRAFT_PREFIX) !== 0) return null;
    if (typeof v.id !== 'string' || !ID_RE.test(v.id)) v.id = '';
    return v;
  }

  // settle decides what a page load does with a marker (null: none).
  // page: {sent: arrived by the send's redirect, compose: a compose page,
  // key: its draft key, error: it carries an error banner}. Returns
  // {drop: draft key to delete or null, clear: remove the marker, id: the
  // marker's copy of the draft id, for a draft that lost its own, or null}.
  function settle(marker, page) {
    var keep = { drop: null, clear: false, id: null };
    if (!marker) return keep;
    if (page.sent) return { drop: marker.key, clear: true, id: null };
    if (!page.compose || page.key !== marker.key) return keep; // the POST may still land
    // The server re-rendered a failed send, with its id: done.
    if (page.error) return { drop: null, clear: true, id: null };
    // Back on the draft without an answer: keep it. Its id makes a resend
    // the same send, which the server answers from its record if the
    // first went out after all.
    return { drop: null, clear: false, id: marker.id || null };
  }

  var C = {
    htmlToText: htmlToText, quote: quote, tokenAt: tokenAt, replaceToken: replaceToken,
    parseDraft: parseDraft, sameFields: sameFields, prune: prune, draftID: draftID, ID_KEEP: ID_KEEP, persistForSend: persistForSend, capDrafts: capDrafts, submitDraft: submitDraft,
    parseMarker: parseMarker, settle: settle, SENDING_KEY: SENDING_KEY,
    DRAFT_PREFIX: DRAFT_PREFIX, DRAFT_CAP: DRAFT_CAP, FIELDS: FIELDS,
  };
  if (typeof module === 'object' && module.exports) { module.exports = C; return; }
  var Pneu = (root.Pneu = root.Pneu || {});
  Pneu.compose = C;

  function session(fn) { try { return fn(window.sessionStorage); } catch (e) { return null; } }
  function local(fn) { try { return fn(window.localStorage); } catch (e) { return null; } }
  function marker() { return parseMarker(session(function (s) { return s.getItem(SENDING_KEY); })); }

  function apply(r) {
    if (r.drop) local(function (s) { s.removeItem(r.drop); });
    if (r.clear) session(function (s) { s.removeItem(SENDING_KEY); });
  }

  // The send's redirect: runs now, from <head>, before app.js reads the URL.
  if (location.hash === '#sent') {
    history.replaceState(history.state, '', location.pathname + location.search);
    apply(settle(marker(), { sent: true }));
    // app.js's stored flash (its FLASH_KEY), shown when it loads just after.
    session(function (s) { s.setItem('pneu:flash', JSON.stringify({ text: 'Sent', at: Date.now() })); });
  }

  // ===========================================================================
  // Browser wiring. Everything below runs only on main.compose.

  function init() {
    var form = document.querySelector('main.compose form[data-draft-key]');
    if (!form) return;
    var el = {};
    FIELDS.forEach(function (f) { el[f] = form.elements.namedItem(f); });
    var bodyEl = el.body;
    var replying = !!(form.elements.namedItem('in_reply_to') || {}).value;
    var hadError = !!document.querySelector('main.compose .error');

    function flash(text, kind) { if (Pneu.flash) Pneu.flash(text, kind); }

    // ---- drafts ----

    var key = DRAFT_PREFIX + form.dataset.draftKey;
    var submitting = false;
    var saveTimer = 0;
    var store = local;
    var idEl = form.elements.namedItem('message_id');
    var idAt = Date.now();
    var sent = false; // submitted at least once: the draft keeps its id however it reads

    function current() {
      var v = {};
      FIELDS.forEach(function (f) { v[f] = el[f] ? el[f].value : ''; });
      return v;
    }
    function server() {
      var v = {};
      FIELDS.forEach(function (f) { v[f] = el[f] ? el[f].defaultValue : ''; });
      return v;
    }

    function save() {
      clearTimeout(saveTimer);
      if (submitting) return;
      var v = current();
      store(function (s) {
        if (sameFields(v, server()) && !hadError && !sent) { s.removeItem(key); return; }
        v.at = Date.now();
        if (idEl && idEl.value) { v.id = idEl.value; v.idAt = idAt; }
        v.sent = sent;
        capDrafts(s, key);
        s.setItem(key, JSON.stringify(v));
      });
    }

    function saveSoon() { clearTimeout(saveTimer); saveTimer = setTimeout(save, 300); }

    var pending = settle(marker(), { compose: true, key: key, error: hadError });
    apply(pending);

    var restored = false;
    var d = parseDraft(store(function (s) { return s.getItem(key); }));
    if (d) sent = d.sent;
    if (hadError) {
      // The server re-rendered what was sent, under the id it was sent
      // with: that is the draft now.
      if (d && idEl && d.id === idEl.value) idAt = d.idAt;
      save();
    } else {
      var id = draftID(d, Date.now());
      if (id && idEl) { idEl.value = id; idAt = d.idAt; }
      else if (pending.id && idEl) { idEl.value = pending.id; sent = true; } // the draft lost it; the marker kept it
      if (d && !sameFields(d, server())) {
        FIELDS.forEach(function (f) { if (el[f]) el[f].value = d[f]; });
        restored = true;
        flash('Draft restored');
      }
    }

    form.addEventListener('input', saveSoon);
    window.addEventListener('pagehide', function () { if (saveTimer) save(); });
    var sendBtn = form.querySelector('button.send');
    var sendLabel = sendBtn ? sendBtn.textContent : '';
    function sending(on) {
      submitting = on;
      form.classList.toggle('sending', on);
      // inert, not disabled: disabled fields would drop out of the POST.
      form.inert = on;
      if (on) form.setAttribute('aria-busy', 'true'); else form.removeAttribute('aria-busy');
      if (sendBtn) sendBtn.textContent = on ? 'Sending' : sendLabel;
    }
    form.addEventListener('submit', function (e) {
      // The draft, id and all, stays until the send's redirect lands
      // (settle); the server keeps the id's record from now. It must be on
      // disk first, or a reopened draft would be a second send.
      clearTimeout(saveTimer);
      var now = Date.now();
      var v = current();
      v.at = now;
      v.id = idEl ? idEl.value : '';
      v.idAt = now;
      v.sent = true;
      var ok = false;
      try { ok = submitDraft(window.localStorage, window.sessionStorage, key, v, now); } catch (err) { ok = false; }
      if (!ok) {
        e.preventDefault();
        flash('Not sent: pneu couldn’t save the draft first, so it couldn’t make sure it sends only once. Free some browser storage and try again.', 'error');
        return;
      }
      sent = true;
      idAt = now;
      arm(false);
      sending(true);
    });
    // A page restored from the back-forward cache after a failed navigation
    // is editable again.
    window.addEventListener('pageshow', function (e) { if (e.persisted) sending(false); });

    // ---- keys ----

    // Holding Ctrl (or Cmd) lights Send: Enter now sends. Any other key
    // pressed under it (Ctrl+C, Ctrl+A) is a different chord and unlights it
    // until the modifier is pressed again.
    function arm(on) { form.classList.toggle('armed', on && !submitting); }
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Control' || e.key === 'Meta') arm(!e.repeat || form.classList.contains('armed'));
      else if (e.key !== 'Enter') arm(false);
    });
    document.addEventListener('keyup', function (e) {
      if (e.key === 'Control' || e.key === 'Meta' || !(e.ctrlKey || e.metaKey)) arm(false);
    });
    window.addEventListener('blur', function () { arm(false); });

    // ---- discard ----

    // Discard drops the draft and leaves: back to where compose was opened
    // from, or the inbox when there is no page to go back to (a fresh window,
    // or a failed send's re-render, whose Back is the compose form again).
    function discard() {
      if (submitting) return;
      clearTimeout(saveTimer);
      submitting = true; // no pagehide save
      store(function (s) { s.removeItem(key); });
      if (history.length > 1 && !hadError) history.back();
      else location.replace('/');
    }
    var discardBtn = form.querySelector('button.discard');
    if (discardBtn) discardBtn.addEventListener('click', discard);
    // Esc in a field blurs it (app.js); Esc with nothing focused discards,
    // so a second Esc leaves.
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape' || e.defaultPrevented || e.isComposing) return;
      if (e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return;
      if (document.querySelector('dialog[open]')) return;
      // The target, not activeElement: app.js has already blurred the field.
      var t = e.target;
      if (t && t.nodeType === 1 && t.closest('input, textarea, select')) return;
      e.preventDefault();
      discard();
    });

    form.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && (e.ctrlKey || e.metaKey) && !e.isComposing) {
        e.preventDefault();
        if (submitting) return;
        if (form.requestSubmit) form.requestSubmit(); else form.submit();
        return;
      }
      // Enter in a one-line field would send the message; it never should.
      if (e.key === 'Enter' && !e.defaultPrevented && e.target.nodeName === 'INPUT') e.preventDefault();
    });

    // ---- Cc/Bcc ----

    var toggle = form.querySelector('button.ccbcc-toggle');
    var rows = [form.querySelector('.field.cc'), form.querySelector('.field.bcc')].filter(Boolean);
    function revealCc() {
      rows.forEach(function (r) { r.hidden = false; });
      if (toggle) toggle.hidden = true;
    }
    rows.forEach(function (r) {
      var input = r.querySelector('input');
      if (input && !input.value.trim()) r.hidden = true;
    });
    if (toggle) {
      toggle.hidden = !rows.some(function (r) { return r.hidden; });
      toggle.addEventListener('click', function () {
        revealCc();
        if (el.cc) el.cc.focus();
      });
    }

    // ---- caret ----

    if (replying) {
      bodyEl.focus();
      if (!restored) { bodyEl.setSelectionRange(0, 0); bodyEl.scrollTop = 0; }
    } else if (el.to && !el.to.value) {
      el.to.focus();
    }

    // ---- HTML-only quote ----

    var quoteFrom = bodyEl.dataset.quoteFrom;
    if (quoteFrom && !restored && !hadError) {
      var tail = bodyEl.defaultValue.replace(/^\n+/, ''); // the attribution line
      fetch(quoteFrom, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
        .then(function (res) {
          if (!res.ok) throw new Error('HTTP ' + res.status);
          return res.json();
        })
        .then(function (data) {
          // Sanitize through mailframe.js's own pipeline (the same DOMPurify
          // config the frame uses, not a copy that could drift), then re-parse
          // its output in an inert DOMParser document: nothing in it loads or
          // runs, and only text is read back out.
          var built = Pneu.assembleMailDocument(data.html || '', data.cids || {},
            document.body.dataset.origin || location.origin);
          var doc = new DOMParser().parseFromString(built.srcdoc, 'text/html');
          var q = quote(htmlToText(doc.body));
          if (!q) return;
          var v = bodyEl.value;
          // Untouched, or typed into above the attribution only: append.
          if (v !== bodyEl.defaultValue && v.slice(-tail.length) !== tail) return;
          var a = bodyEl.selectionStart, b = bodyEl.selectionEnd, top = bodyEl.scrollTop;
          bodyEl.value = v + q;
          bodyEl.setSelectionRange(a, b);
          bodyEl.scrollTop = top;
          if (v !== bodyEl.defaultValue) saveSoon();
        })
        .catch(function (err) { flash('Could not quote the original: ' + err.message, 'error'); });
    }

    // ---- address autocomplete ----

    Array.prototype.forEach.call(form.querySelectorAll('input[data-addresses]'), function (input) {
      autocomplete(input, input.dataset.addresses);
    });

    function autocomplete(input, url) {
      var list = document.createElement('ul');
      list.className = 'suggest';
      list.setAttribute('role', 'listbox');
      list.id = input.id + '-suggest';
      list.hidden = true;
      input.parentNode.appendChild(list);
      input.setAttribute('role', 'combobox');
      input.setAttribute('aria-autocomplete', 'list');
      input.setAttribute('aria-controls', list.id);
      input.setAttribute('aria-expanded', 'false');

      var timer = 0, ctl = null, options = [], active = -1;

      function close() {
        clearTimeout(timer);
        if (ctl) { ctl.abort(); ctl = null; }
        list.hidden = true;
        list.textContent = '';
        options = [];
        active = -1;
        input.setAttribute('aria-expanded', 'false');
        input.removeAttribute('aria-activedescendant');
      }

      function setActive(i) {
        if (!options.length) return;
        if (options[active]) options[active].li.classList.remove('active');
        active = (i + options.length) % options.length;
        var li = options[active].li;
        li.classList.add('active');
        li.setAttribute('aria-selected', 'true');
        options.forEach(function (o, j) { if (j !== active) o.li.setAttribute('aria-selected', 'false'); });
        input.setAttribute('aria-activedescendant', li.id);
        li.scrollIntoView({ block: 'nearest' });
      }

      function choose(i) {
        var o = options[i];
        if (!o) return;
        var r = replaceToken(input.value, tokenAt(input.value, input.selectionStart), o.value);
        close();
        input.value = r.value;
        input.setSelectionRange(r.caret, r.caret);
        saveSoon();
      }

      function show(q, found) {
        list.textContent = '';
        options = [];
        active = -1;
        found.slice(0, 8).forEach(function (s, i) {
          if (typeof s !== 'string') return;
          var li = document.createElement('li');
          li.setAttribute('role', 'option');
          li.id = list.id + '-' + i;
          li.textContent = s;
          li.addEventListener('mousedown', function (e) { e.preventDefault(); choose(options.indexOf(o)); });
          var o = { li: li, value: s };
          options.push(o);
          list.appendChild(li);
        });
        if (!options.length) { close(); return; }
        list.hidden = false;
        input.setAttribute('aria-expanded', 'true');
        setActive(0);
      }

      function lookup() {
        var tok = tokenAt(input.value, input.selectionStart);
        if (ctl) { ctl.abort(); ctl = null; }
        clearTimeout(timer);
        if (!tok.query) { close(); return; }
        timer = setTimeout(function () {
          var c = (ctl = new AbortController());
          fetch(url + '?q=' + encodeURIComponent(tok.query), {
            credentials: 'same-origin', headers: { Accept: 'application/json' }, signal: c.signal,
          }).then(function (res) {
            if (!res.ok) throw new Error('HTTP ' + res.status);
            return res.json();
          }).then(function (found) {
            if (c !== ctl || document.activeElement !== input) return;
            ctl = null;
            if (tokenAt(input.value, input.selectionStart).query !== tok.query) return;
            show(tok.query, Array.isArray(found) ? found : []);
          }).catch(function (err) {
            if (err && err.name === 'AbortError') return;
            close();
          });
        }, 150);
      }

      input.addEventListener('input', lookup);
      input.addEventListener('blur', close);
      input.addEventListener('keydown', function (e) {
        if (list.hidden || e.isComposing) return;
        switch (e.key) {
          case 'ArrowDown': setActive(active + 1); break;
          case 'ArrowUp': setActive(active - 1); break;
          case 'Enter':
            if (e.ctrlKey || e.metaKey) return; // Ctrl+Enter still sends
            choose(active); break;
          case 'Tab':
            if (e.shiftKey || e.altKey || e.ctrlKey) return;
            choose(active); break;
          case 'Escape': close(); break;
          default: return;
        }
        e.preventDefault(); // app.js skips prevented keys, so Escape doesn't also blur
      });
    }
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})(this);
