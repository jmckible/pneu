// viewer.js — the attachment viewer: a modal over the thread that shows one
// attachment at a time and steps through every viewable one in the thread.
// The server decides each attachment's kind (data-view, attach.go); this file
// only draws it. The pure half (CSV, iCalendar, charsets) also runs under
// node --test (web/viewer.test.js); the DOM half only in the browser.
//
// Nothing from an attachment reaches this document as markup: names, text,
// table cells and calendar fields go through textContent. HTML attachments
// and rendered Markdown go through Pneu.renderMailFrame into the sandboxed
// srcdoc frame, like a mail body. SVG is shown only as an <img>.
(function (root) {
  'use strict';

  var TEXT_CAP = 2 * 1024 * 1024;
  var CSV_CAP = 16 * 1024 * 1024; // parsing stops at CSV_ROWS or CSV_CELLS; this bounds the fetch
  var CSV_ROWS = 2000;
  var CSV_COLS = 200;
  var CSV_CELLS = 200000; // rows × columns, whichever runs out first

  // ---- pure -----------------------------------------------------------------

  // charset is a Content-Type's charset label, utf-8 if it names none.
  function charset(ctype) {
    var m = /;\s*charset\s*=\s*"?([^";\s]+)"?/i.exec(ctype || '');
    return m ? m[1] : 'utf-8';
  }

  // decode turns bytes into text by the response's charset; a label
  // TextDecoder doesn't know falls back to utf-8.
  function decode(buf, ctype) {
    var d;
    try { d = new TextDecoder(charset(ctype)); } catch (e) { d = new TextDecoder('utf-8'); }
    return d.decode(buf);
  }

  // parseCSV reads RFC 4180 (quoted fields, "" escapes, CRLF or LF, line
  // breaks inside quotes), leniently: a quote mid-field is literal. It keeps
  // at most maxCols fields of a row and maxCells in all, skipping the rest of
  // a row without building it; the row that spends the last cell is the last.
  // rowsCut and colsCut say what was left out.
  function parseCSV(text, delim, maxRows, maxCols, maxCells) {
    var rows = [], row = [], field = '', started = false, quoted = false, n = text.length;
    var cells = 0, rowsCut = false, colsCut = false;
    var i = text.charCodeAt(0) === 0xfeff ? 1 : 0;
    // keep is how many more fields this row may take; past it, fields are
    // parsed (quotes still decide where the row ends) but not kept.
    var keep = Math.min(maxCols, maxCells);
    function push() {
      if (keep > 0) {
        row.push(field);
        keep--;
        cells++;
      } else if (row.length === maxCols) {
        colsCut = true;
      } else {
        rowsCut = true; // the cell budget ran out mid-row
      }
      field = '';
      started = false;
    }
    function endRow() {
      push();
      rows.push(row);
      row = [];
      keep = Math.min(maxCols, maxCells - cells);
    }
    while (i < n) {
      var c = text[i];
      if (quoted) {
        if (c === '"') {
          if (text[i + 1] === '"') { if (keep > 0) field += '"'; i += 2; continue; }
          quoted = false;
        } else if (keep > 0) {
          field += c;
        }
        i++;
        continue;
      }
      if (c === '"' && !started) {
        quoted = started = true;
      } else if (c === delim) {
        push();
      } else if (c === '\r' || c === '\n') {
        if (c === '\r' && text[i + 1] === '\n') i++;
        endRow();
        if (rows.length === maxRows || cells === maxCells) {
          return { rows: rows, rowsCut: rowsCut || i + 1 < n, colsCut: colsCut };
        }
      } else {
        if (keep > 0) field += c;
        started = true;
      }
      i++;
    }
    if (started || row.length) endRow();
    return { rows: rows, rowsCut: rowsCut, colsCut: colsCut };
  }

  // ---- iCalendar (RFC 5545), just enough to say what an invite is for.

  // icsLine splits NAME;PARAM=V;PARAM="a:b":value; the name's colon is the
  // first one outside quotes.
  function icsLine(line) {
    var q = false, i = 0;
    for (; i < line.length; i++) {
      if (line[i] === '"') q = !q;
      else if (line[i] === ':' && !q) break;
    }
    if (i >= line.length) return null;
    var head = line.slice(0, i).match(/(?:[^;"]|"[^"]*")+/g) || [''];
    var params = {};
    head.slice(1).forEach(function (p) {
      var eq = p.indexOf('=');
      if (eq > 0) params[p.slice(0, eq).toUpperCase()] = p.slice(eq + 1).replace(/^"|"$/g, '');
    });
    return { name: head[0].toUpperCase(), params: params, value: line.slice(i + 1) };
  }

  function icsText(v) {
    return v.replace(/\\([nN\\,;])/g, function (_, c) { return c === 'n' || c === 'N' ? '\n' : c; });
  }

  // firstEvent is the first VEVENT's properties by name (the first of each),
  // leaving out those of components nested in it (VALARM), or null.
  function firstEvent(text) {
    var lines = text.replace(/\r\n?/g, '\n').replace(/\n[ \t]/g, '').split('\n');
    var ev = null, depth = 0;
    for (var i = 0; i < lines.length; i++) {
      var l = icsLine(lines[i]);
      if (!l) continue;
      var v = l.value.trim().toUpperCase();
      if (l.name === 'BEGIN') {
        if (ev) depth++;
        else if (v === 'VEVENT') ev = {};
      } else if (l.name === 'END') {
        if (!ev) continue;
        if (depth) depth--;
        else if (v === 'VEVENT') return ev;
      } else if (ev && !depth && !Object.prototype.hasOwnProperty.call(ev, l.name)) {
        ev[l.name] = l;
      }
    }
    return ev;
  }

  var DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  function fmt(y, mo, d, dow, h, mi) {
    var out = DAYS[dow] + ', ' + MONTHS[mo] + ' ' + d + ', ' + y;
    if (h === undefined) return out;
    return out + ', ' + ((h + 11) % 12 + 1) + ':' + (mi < 10 ? '0' : '') + mi + (h < 12 ? ' AM' : ' PM');
  }

  // icsDate reads a DATE or DATE-TIME property: {date, allDay, text}. A UTC
  // time (Z) shows in local time; a TZID or floating time shows as written,
  // with the TZID named (no tz database here to convert it).
  function icsDate(p) {
    var m = /^(\d{4})(\d{2})(\d{2})(?:T(\d{2})(\d{2})(\d{2})?(Z)?)?$/.exec(p.value.trim());
    if (!m) return { text: p.value.trim() };
    var y = +m[1], mo = +m[2] - 1, d = +m[3];
    if (m[4] === undefined) {
      var day = new Date(Date.UTC(y, mo, d));
      return { date: day, allDay: true, text: fmt(y, mo, d, day.getUTCDay()) };
    }
    var h = +m[4], mi = +m[5], s = +(m[6] || 0);
    var t = new Date(Date.UTC(y, mo, d, h, mi, s));
    if (m[7]) {
      return { date: t, text: fmt(t.getFullYear(), t.getMonth(), t.getDate(), t.getDay(), t.getHours(), t.getMinutes()) };
    }
    var text = fmt(y, mo, d, t.getUTCDay(), h, mi);
    if (p.params.TZID) text += ' (' + p.params.TZID + ')';
    return { date: t, text: text };
  }

  // icsFields is the invite as [label, text] rows, or null without an event.
  function icsFields(text) {
    var ev = firstEvent(text);
    if (!ev) return null;
    var rows = [];
    function add(label, v) { if (v) rows.push([label, v]); }
    add('Event', ev.SUMMARY && icsText(ev.SUMMARY.value));
    var start = ev.DTSTART && icsDate(ev.DTSTART);
    var end = ev.DTEND && icsDate(ev.DTEND);
    add(start && start.allDay ? 'Date' : 'Starts', start && start.text);
    if (end && end.allDay && start && start.allDay && end.date && start.date) {
      // An all-day DTEND is exclusive: a one-day event ends the next day.
      var last = new Date(end.date.getTime() - 86400000);
      if (last > start.date) add('Until', fmt(last.getUTCFullYear(), last.getUTCMonth(), last.getUTCDate(), last.getUTCDay()));
    } else if (end) {
      add('Ends', end.text);
    }
    add('Repeats', ev.RRULE && ev.RRULE.value);
    add('Where', ev.LOCATION && icsText(ev.LOCATION.value));
    if (ev.ORGANIZER) {
      var addr = ev.ORGANIZER.value.replace(/^mailto:/i, '');
      var cn = ev.ORGANIZER.params.CN;
      add('Organizer', cn ? cn + ' <' + addr + '>' : addr);
    }
    add('Details', ev.DESCRIPTION && icsText(ev.DESCRIPTION.value));
    return rows;
  }

  function size(n) {
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(n < 10240 ? 1 : 0) + ' KB';
    return (n / 1024 / 1024).toFixed(1) + ' MB';
  }

  var V = {
    TEXT_CAP: TEXT_CAP, CSV_ROWS: CSV_ROWS, CSV_COLS: CSV_COLS, CSV_CELLS: CSV_CELLS, charset: charset, decode: decode, parseCSV: parseCSV,
    icsFields: icsFields, size: size,
  };
  if (typeof module === 'object' && module.exports) {
    module.exports = V;
    return;
  }

  // ---- DOM ------------------------------------------------------------------
  // open(ctx) shows ctx.items[ctx.index]. ctx (from app.js):
  //   items          [{ href, name, kind, inline }] in document order
  //   origin         the app origin, for renderMailFrame
  //   theme(el)      the frame theme for a body painted on el (app.js frameTheme)
  //   loadMailFrame  resolves once Pneu.renderMailFrame exists
  //   onClose        called once the dialog has closed

  var dlg = null, els = {}, state = null, markedP = null;

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  function button(key, label, fn) {
    var b = el('button');
    b.type = 'button';
    b.appendChild(el('kbd', '', key));
    b.appendChild(document.createTextNode(' ' + label));
    b.addEventListener('click', fn);
    return b;
  }

  function build() {
    dlg = el('dialog');
    dlg.id = 'viewer';
    dlg.tabIndex = -1;
    var head = el('header');
    els.name = el('span', 'name');
    els.pos = el('span', 'pos');
    // Chromium's PDF viewer keeps the keys it's given; say how to get them back.
    els.hint = el('span', 'hint', 'Keys go to the PDF while it has focus — click outside it for n/p/Esc');
    els.prev = button('p', 'Previous', step(-1));
    els.next = button('n', 'Next', step(1));
    els.download = button('d', 'Download', download);
    els.tab = button('o', 'Open in tab', openTab);
    var actions = el('span', 'actions');
    actions.append(els.prev, els.next, els.download, els.tab, button('Esc', 'Close', close));
    head.append(els.name, els.pos, els.hint, actions);
    els.stage = el('div', 'stage');
    dlg.append(head, els.stage);
    dlg.addEventListener('keydown', onKey);
    dlg.addEventListener('close', closed);
    // A click on the backdrop (the dialog box itself is covered by its
    // children) closes, as the help overlay does.
    dlg.addEventListener('click', function (e) { if (e.target === dlg) close(); });
    // A click anywhere in the dialog but a control takes the keys back from
    // a frame or player (the PDF viewer never hands them on).
    dlg.addEventListener('pointerdown', function (e) {
      if (e.target.closest && !e.target.closest('button, a, input, select, textarea, video, audio, iframe')) {
        dlg.focus({ preventScroll: true });
      }
    });
    document.body.appendChild(dlg);
  }

  function current() { return state && state.items[state.index]; }

  // onKey takes the dialog's keys, and those typed inside its mail frames
  // (renderMailFrame's onKeydown). onKey in app.js stands aside while a
  // dialog is open; stopPropagation keeps a key that closes this one (Esc,
  // q) from reaching it afterwards.
  function onKey(e) {
    if (!state || e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return;
    var t = e.target;
    // A focused player keeps its arrows for seeking.
    var media = t && t.nodeType === 1 && t.closest('video, audio');
    var fn = null;
    switch (e.key) {
      case 'n': fn = step(1); break;
      case 'p': fn = step(-1); break;
      case 'ArrowRight': if (!media) fn = step(1); break;
      case 'ArrowLeft': if (!media) fn = step(-1); break;
      case 'd': fn = download; break;
      case 'o': fn = openTab; break;
      case 'q': case 'Escape': fn = close; break;
    }
    if (!fn) return;
    e.preventDefault();
    e.stopPropagation();
    fn();
  }

  function step(dir) {
    return function () {
      var i = state.index + dir;
      if (i >= 0 && i < state.items.length) show(i);
    };
  }

  function download() {
    var it = current();
    if (!it) return;
    // Inside the dialog: everything outside a modal is inert.
    var a = el('a');
    a.href = it.href;
    a.download = it.name;
    a.hidden = true;
    dlg.appendChild(a);
    a.click();
    a.remove();
  }

  // openTab: only what /part serves inline (inlineTypes) would show there.
  function openTab() {
    var it = current();
    if (it && it.inline) window.open(it.href, '_blank', 'noopener');
  }

  function close() { if (dlg && dlg.open) dlg.close(); }

  function closed() {
    if (state && state.abort) state.abort.abort();
    els.stage.replaceChildren(); // stops a playing video
    var ctx = state && state.ctx;
    state = null;
    if (ctx && ctx.onClose) ctx.onClose();
  }

  function note(text) {
    var p = el('p', 'note', text);
    els.stage.appendChild(p);
    return p;
  }

  // unshown: a note, and the download that remains.
  function unshown(text) {
    var p = note(text + ' ');
    p.appendChild(button('d', 'Download', download));
  }

  function show(i) {
    var s = state;
    var it = s.items[i];
    s.index = i;
    if (s.abort) s.abort.abort();
    s.abort = new AbortController();
    var signal = s.abort.signal;
    els.name.textContent = it.name;
    els.name.title = it.name;
    els.pos.textContent = (i + 1) + ' of ' + s.items.length;
    els.tab.hidden = !it.inline;
    els.hint.hidden = it.kind !== 'pdf';
    // Focus in the outgoing content (a mail frame, a player), or on a button
    // about to be disabled, would fall to the inert page and take the keys
    // with it.
    var atEnd = (els.prev === document.activeElement && i === 0) ||
      (els.next === document.activeElement && i === s.items.length - 1);
    if (atEnd || els.stage.contains(document.activeElement)) dlg.focus();
    els.prev.disabled = i === 0;
    els.next.disabled = i === s.items.length - 1;
    els.stage.replaceChildren();
    els.stage.dataset.kind = it.kind;
    els.stage.scrollTop = 0;
    var render = RENDER[it.kind];
    if (!render) {
      unshown('No preview for this file.');
      return;
    }
    Promise.resolve().then(function () { return render(it, signal); }).catch(function (err) {
      if (signal.aborted) return;
      els.stage.replaceChildren();
      unshown('Could not show ' + it.name + ': ' + err.message + '.');
    });
  }

  // fetchText reads a part as text, or {big: bytes} past cap.
  function fetchText(href, cap, signal) {
    return fetch(href, { credentials: 'same-origin', signal: signal }).then(function (res) {
      if (!res.ok) throw new Error('HTTP ' + res.status);
      var ctype = res.headers.get('Content-Type') || '';
      var len = parseInt(res.headers.get('Content-Length'), 10);
      if (len > cap) {
        if (res.body) res.body.cancel();
        return { big: len };
      }
      return res.arrayBuffer().then(function (buf) {
        if (buf.byteLength > cap) return { big: buf.byteLength };
        return { text: decode(buf, ctype), ctype: ctype };
      });
    });
  }

  // withText runs fn on the part's text once it arrives, if this is still
  // the attachment on show.
  function withText(it, cap, signal, fn) {
    return fetchText(it.href, cap, signal).then(function (got) {
      if (signal.aborted) return null;
      if (got.big) {
        unshown(it.name + ' is ' + size(got.big) + ', too large to show here.');
        return null;
      }
      return fn(got);
    });
  }

  function loadMarked() {
    if (root.marked) return Promise.resolve(root.marked);
    if (!markedP) {
      markedP = new Promise(function (resolve, reject) {
        var s = document.createElement('script');
        s.src = '/static/marked.umd.js';
        s.onload = function () { resolve(root.marked); };
        s.onerror = function () { markedP = null; reject(new Error('could not load marked')); };
        document.head.appendChild(s);
      });
    }
    return markedP;
  }

  // frame paints untrusted HTML the way app.js paints a mail body: the
  // sandboxed srcdoc frame, in the app's colors unless the document has its
  // own, remote images behind a click.
  function frame(html) {
    var ctx = state.ctx;
    var stage = els.stage;
    var mail = { frame: null, remoteImages: false };
    function paint() {
      var built = Pneu.renderMailFrame(html, {}, ctx.origin, {
        frame: mail.frame || undefined, onKeydown: onKey, remoteImages: mail.remoteImages, theme: ctx.theme(stage),
      });
      mail.frame = built.frame;
      return built;
    }
    var built = paint();
    if (built.remote) {
      var btn = el('button', 'remote-images', 'Load remote images');
      btn.type = 'button';
      btn.addEventListener('click', function () {
        mail.remoteImages = true;
        paint();
        btn.remove();
      });
      stage.appendChild(btn);
    }
    stage.appendChild(built.frame);
  }

  function table(head, rows) {
    var t = el('table');
    if (head) {
      var tr = el('tr');
      head.forEach(function (c) { tr.appendChild(el('th', '', c)); });
      t.appendChild(el('thead')).appendChild(tr);
    }
    var tb = t.appendChild(el('tbody'));
    rows.forEach(function (r) {
      var tr = el('tr');
      r.forEach(function (c) {
        var td = el('td', '', c.text !== undefined ? c.text : c);
        if (c.cls) td.className = c.cls;
        tr.appendChild(td);
      });
      tb.appendChild(tr);
    });
    return t;
  }

  var RENDER = {
    image: function (it) {
      var img = el('img');
      img.alt = it.name;
      img.addEventListener('error', function () {
        img.remove();
        unshown('Could not display this image.');
      });
      img.src = it.href;
      els.stage.appendChild(img);
    },
    // Chromium's own viewer. It can't run under a sandbox, so this frame is
    // the one unsandboxed one; /part lets only the app frame a PDF.
    pdf: function (it) {
      var f = el('iframe');
      f.title = it.name;
      f.src = it.href;
      els.stage.appendChild(f);
    },
    video: media,
    audio: media,
    text: function (it, signal) {
      return withText(it, TEXT_CAP, signal, function (got) {
        els.stage.appendChild(el('pre', '', got.text));
      });
    },
    csv: function (it, signal) {
      return withText(it, CSV_CAP, signal, function (got) {
        var tsv = /\.tsv$/i.test(it.name) || /tab-separated/i.test(got.ctype);
        var parsed = parseCSV(got.text, tsv ? '\t' : ',', CSV_ROWS + 1, CSV_COLS, CSV_CELLS);
        var rows = parsed.rows;
        if (!rows.length) { note('Empty file.'); return; }
        var cut = [];
        if (parsed.rowsCut) cut.push((rows.length - 1) + ' rows');
        if (parsed.colsCut) cut.push(CSV_COLS + ' columns');
        if (cut.length) note('Showing the first ' + cut.join(' and ') + '.');
        els.stage.appendChild(table(rows[0], rows.slice(1)));
      });
    },
    markdown: function (it, signal) {
      return Promise.all([withText(it, TEXT_CAP, signal, function (got) { return got; }), loadMarked(), state.ctx.loadMailFrame()])
        .then(function (r) {
          if (!r[0] || signal.aborted) return;
          var html = r[1].parse(r[0].text, { gfm: true, async: false });
          // A reading measure; the frame's own style sets the font.
          frame('<div style="max-width:46em;margin:0 auto">' + html + '</div>');
        });
    },
    html: function (it, signal) {
      return Promise.all([withText(it, TEXT_CAP, signal, function (got) { return got; }), state.ctx.loadMailFrame()])
        .then(function (r) {
          if (r[0] && !signal.aborted) frame(r[0].text);
        });
    },
    ics: function (it, signal) {
      return withText(it, TEXT_CAP, signal, function (got) {
        var rows = icsFields(got.text);
        if (!rows || !rows.length) { unshown('No event in this calendar file.'); return; }
        var dl = el('dl');
        rows.forEach(function (r) {
          dl.appendChild(el('dt', '', r[0]));
          dl.appendChild(el('dd', '', r[1]));
        });
        els.stage.appendChild(dl);
      });
    },
    zip: function (it, signal) {
      return fetch(it.href + '/zip', { credentials: 'same-origin', signal: signal, headers: { Accept: 'application/json' } })
        .then(function (res) {
          return res.json().catch(function () { return null; }).then(function (data) {
            if (!res.ok || !data || !data.entries) throw new Error((data && data.error) || 'HTTP ' + res.status);
            return data;
          });
        })
        .then(function (data) {
          if (signal.aborted) return;
          if (data.tooMany) {
            unshown('Too many entries to list' + (data.total ? ' (' + data.total + ')' : '') + '.');
            return;
          }
          if (data.truncated) note('Showing the first ' + data.entries.length + ' of ' + data.total + ' entries.');
          els.stage.appendChild(table(['Name', 'Size', 'Modified'], data.entries.map(function (e) {
            return [
              { text: e.name, cls: e.dir ? 'dir' : '' },
              { text: e.dir ? '' : size(e.size), cls: 'num' },
              e.modified ? e.modified.slice(0, 16).replace('T', ' ') : '',
            ];
          })));
        });
    },
  };

  function media(it) {
    var m = el(it.kind);
    m.controls = true;
    m.preload = 'metadata';
    m.addEventListener('error', function () {
      m.remove();
      unshown('This browser can’t play it.');
    });
    m.src = it.href;
    els.stage.appendChild(m);
  }

  V.open = function (ctx) {
    if (!dlg) build();
    if (state && state.abort) state.abort.abort();
    state = { ctx: ctx, items: ctx.items, index: 0, abort: null };
    if (!dlg.open) dlg.showModal();
    dlg.focus();
    show(Math.max(0, Math.min(ctx.items.length - 1, ctx.index)));
  };

  (root.Pneu = root.Pneu || {}).viewer = V;
})(this);
