// triage.js — the pure half of triage: POST /tag bodies, selection after a
// removal, the undo stack, id normalisation. No DOM, no fetch; app.js wires
// these to the page and web/triage.test.js runs them under node --test.
(function (root) {
  'use strict';

  // The server's undoDepth (tag.go). Both hold only undoable actions (read-on-
  // open is on neither), so an id this stack keeps is still on the server.
  var UNDO_CAP = 20;
  var MAX_IDS = 500; // tag.go's maxTagIDs

  // body builds the urlencoded form tag.go reads. ids are passed through as
  // the page carries them (data-msgids: url.QueryEscape'd, data-msgid:
  // url.PathEscape'd); the server PathUnescapes each token after the form
  // layer has undone URLSearchParams' own encoding.
  function body(action, opts) {
    opts = opts || {};
    var p = new URLSearchParams();
    p.set('action', action);
    if (action === 'undo') {
      if (opts.id) p.set('id', opts.id);
      return p.toString();
    }
    if (opts.account) p.set('account', opts.account);
    var ids = Array.isArray(opts.ids) ? opts.ids.join(' ') : String(opts.ids || '');
    ids = ids.trim().split(/\s+/).filter(Boolean).join(' ');
    if (ids) p.set('ids', ids);
    if (opts.thread) p.set('thread', opts.thread);
    return p.toString();
  }

  // removeArgs is the POST /tag opts for archive or trash of a whole thread:
  // its ids, or, past the server's id cap, the thread id alone (the one case
  // tag.go resolves thread:X itself).
  function removeArgs(account, ids, thread) {
    if (splitIds(ids).length > MAX_IDS && thread) return { account: account, thread: thread };
    return { account: account, ids: ids, thread: thread };
  }

  // splitIds turns a data-msgids value into its tokens.
  function splitIds(v) {
    return String(v || '').trim().split(/\s+/).filter(Boolean);
  }

  // decodeId maps any of the page's encodings (QueryEscape in data-msgids
  // and responses, PathEscape in data-msgid) to the raw Message-ID, so ids
  // from different sources compare equal. Ids never contain spaces, and
  // QueryEscape writes a literal '+' as %2B, so '+' is never a space here.
  function decodeId(s) {
    try { return decodeURIComponent(s); } catch (e) { return s; }
  }

  // nextIndex is the row to select after removing index i, with n rows left:
  // the one that slid into its place, or the new last row at the end.
  function nextIndex(i, n) {
    if (n <= 0) return -1;
    return i < n ? i : n - 1;
  }

  // Undo stack, oldest first, capped. Pure: every call returns a new array.
  function push(stack, entry, cap) {
    cap = cap || UNDO_CAP;
    var out = (Array.isArray(stack) ? stack : []).concat([entry]);
    return out.length > cap ? out.slice(out.length - cap) : out;
  }

  function pop(stack) {
    stack = Array.isArray(stack) ? stack : [];
    if (!stack.length) return { entry: null, stack: [] };
    return { entry: stack[stack.length - 1], stack: stack.slice(0, -1) };
  }

  // parse reads a stored stack, dropping anything malformed.
  function parse(json) {
    var v;
    try { v = JSON.parse(json || '[]'); } catch (e) { return []; }
    if (!Array.isArray(v)) return [];
    return v.filter(function (e) {
      return e && typeof e.id === 'string' && /^[0-9a-f]{1,64}$/.test(e.id) && typeof e.action === 'string';
    }).slice(-UNDO_CAP);
  }

  // removes says whether action takes a thread out of view: archive leaves
  // only the inbox; trash is excluded from every other search (exclude_tags),
  // and the trash view never gets there (skip).
  function removes(action, view) {
    if (action === 'trash') return view !== 'trash';
    if (action === 'archive') return view === 'inbox';
    return false;
  }

  // skip is why action must not be sent from view ('' to send it): trash in
  // trash changes nothing, and trash in spam would leave spam and trash both
  // set, which lieer forbids (one of inbox/spam/trash). view is a list's
  // data-view or a thread's data-in.
  function skip(action, view) {
    if (action !== 'trash') return '';
    if (view === 'trash') return 'Already in trash';
    if (view === 'spam') return 'Spam can\'t be trashed here; Gmail deletes it after 30 days';
    return '';
  }

  // retryAfter reads a Retry-After header in seconds, bounded.
  function retryAfter(v) {
    var n = parseInt(v, 10);
    if (!(n >= 0)) n = 2;
    return Math.min(n, 10) * 1000;
  }

  // ---- split pane (SPEC.md Layout) ----------------------------------------

  // SPLIT_CH is the viewport width, in the app font's ch, at which the list
  // and the thread sit side by side. app.js measures ch and builds the media
  // query from it.
  var SPLIT_CH = 140;

  // The index views' paths (server.go); search keeps its ?q=.
  var LIST_PATHS = { '/': true, '/starred': true, '/sent': true, '/spam': true, '/trash': true, '/all': true, '/search': true };

  // pathKind says what a URL names: 'thread' (/t/<account>/<thread>),
  // 'list' (an index view), or null (compose, reply, anything else).
  function pathKind(url) {
    var path = String(url || '').split(/[?#]/)[0];
    if (/^\/t\/[^/]+\/[^/]+$/.test(path)) return 'thread';
    return Object.prototype.hasOwnProperty.call(LIST_PATHS, path) ? 'list' : null;
  }

  // listURL vets the remembered view (sessionStorage pneu.view) before it
  // is fetched: a same-origin index view, else the inbox.
  function listURL(v) {
    if (typeof v !== 'string' || v.charAt(0) !== '/' || v.charAt(1) === '/' || v.charAt(1) === '\\') return '/';
    return pathKind(v) === 'list' ? v : '/';
  }

  // historyOp is how showing next changes history from the current URL:
  // nothing when it is already there, pushState for an open (Enter, o, l,
  // Tab, a click), replaceState when the pane moves on by itself (the next
  // thread after a removal, the list's URL when the pane empties).
  function historyOp(current, next, push) {
    if (!next || next === current) return 'none';
    return push ? 'push' : 'replace';
  }

  // paneView is the list to show beside a thread loaded directly: the one
  // its history entry was shown beside (state.view, written by the split),
  // else the last list page this tab loaded; either vetted by listURL.
  function paneView(state, stored) {
    var v = state && typeof state.view === 'string' ? listURL(state.view) : '/';
    if (v !== '/' || (state && state.view === '/')) return v;
    return listURL(stored);
  }

  // refreshStale says a list refresh answered after something changed the
  // list it would replace: a key (cursor move, removal, undo) or a tag write
  // since it was sent, or a write still in flight. sent and now are
  // {lastKey, lastTag, inflight}.
  function refreshStale(sent, now) {
    return now.lastKey !== sent.lastKey || now.lastTag !== sent.lastTag || now.inflight > 0;
  }

  // restoreIndex is where a re-rendered list puts its cursor: the row for
  // want's thread (and account, when given) if it is still listed, else the
  // same index, clamped. rows are [{thread, account}]; -1 when empty.
  function restoreIndex(rows, want) {
    var n = rows ? rows.length : 0;
    if (!n) return -1;
    want = want || {};
    if (want.thread) {
      for (var i = 0; i < n; i++) {
        if (rows[i].thread === want.thread && (!want.account || rows[i].account === want.account)) return i;
      }
    }
    var j = parseInt(want.index, 10);
    if (!(j >= 0)) j = 0;
    return Math.min(j, n - 1);
  }

  // cycle is the pane Tab (dir 1) or Shift+Tab (dir -1) hands the keys to
  // from active; with no active pane, the list.
  var PANES = ['list', 'thread'];
  function cycle(active, dir) {
    var i = PANES.indexOf(active);
    if (i < 0) return PANES[0];
    var n = PANES.length;
    return PANES[(((i + (dir < 0 ? -1 : 1)) % n) + n) % n];
  }

  // rgbHex formats 0-255 channels as #rrggbb (the frame's theme colors).
  function rgbHex(r, g, b) {
    return '#' + [r, g, b].map(function (x) {
      x = Math.max(0, Math.min(255, Math.round(Number(x) || 0)));
      return (x < 16 ? '0' : '') + x.toString(16);
    }).join('');
  }

  var LABELS = {
    archive: 'Archived', trash: 'Moved to trash', star: 'Starred',
    unstar: 'Unstarred', unread: 'Marked unread', read: 'Marked read',
  };

  function label(action) { return LABELS[action] || action; }

  // position is the list title's count, as read.go positionText writes it:
  // the row count on a single page, else "51–100 of 1,234" (no " of …"
  // while total is unknown, i.e. negative).
  function position(start, rows, total, paged) {
    var n = function (v) { return Number(v).toLocaleString('en-US'); };
    if (!paged) return n(rows);
    if (!rows) return '0';
    return n(start + 1) + '–' + n(start + rows) + (total >= 0 ? ' of ' + n(total) : '');
  }

  var VERBS = {
    archive: 'Archive', trash: 'Trash', star: 'Star', unstar: 'Unstar', unread: 'Mark unread', read: 'Mark read',
  };

  function verb(action) { return VERBS[action] || action; }

  var T = {
    UNDO_CAP: UNDO_CAP, MAX_IDS: MAX_IDS, body: body, removeArgs: removeArgs, splitIds: splitIds, decodeId: decodeId,
    nextIndex: nextIndex, push: push, pop: pop, parse: parse, removes: removes, skip: skip,
    retryAfter: retryAfter, label: label, verb: verb, position: position,
    SPLIT_CH: SPLIT_CH, pathKind: pathKind, listURL: listURL, historyOp: historyOp, restoreIndex: restoreIndex,
    paneView: paneView, refreshStale: refreshStale, rgbHex: rgbHex, cycle: cycle,
  };
  if (typeof module === 'object' && module.exports) module.exports = T;
  else (root.Pneu = root.Pneu || {}).triage = T;
})(this);
