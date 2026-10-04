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

  // removeArgs is the POST /tag opts for archive, trash or spam of a whole thread:
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
  // only the inbox; trash and spam are excluded from every other search
  // (exclude_tags), and their own views never get there (skip).
  function removes(action, view) {
    if (action === 'trash') return view !== 'trash';
    if (action === 'spam') return view !== 'spam';
    if (action === 'archive') return view === 'inbox';
    return false;
  }

  // skip is why action must not be sent from view ('' to send it): trash in
  // trash or spam in spam changes nothing, and either in the other's view
  // would leave spam and trash both set, which lieer forbids (one of
  // inbox/spam/trash). view is a list's data-view or a thread's data-in.
  function skip(action, view) {
    if (action === 'trash') {
      if (view === 'trash') return 'Already in trash';
      if (view === 'spam') return 'Spam can\'t be trashed here; Gmail deletes it after 30 days';
    }
    if (action === 'spam') {
      if (view === 'spam') return 'Already in spam';
      if (view === 'trash') return 'Trash can\'t be marked spam here';
    }
    return '';
  }

  // retryAfter reads a Retry-After header in seconds, bounded.
  function retryAfter(v) {
    var n = parseInt(v, 10);
    if (!(n >= 0)) n = 2;
    return Math.min(n, 10) * 1000;
  }

  // ---- split pane (SPEC.md Layout) ----------------------------------------

  // The window's widths, in the app font's ch (tube.js measures it; CSS
  // media queries would measure ch in the browser's default font). Every
  // layout threshold is here.
  // SPLIT_CH is the panes' width (the window less the tube) at which the
  // list and the thread sit side by side. TUBE_CH and TUBE_FOLD_CH are the
  // tube's widths, open and folded to its digits (tube.js hands them to
  // app.css as --tube-w and --tube-fold-w). LIST_MIN_CH: a single pane
  // narrower than this with the tube open folds the tube.
  var SPLIT_CH = 140;
  var TUBE_CH = 18;
  var TUBE_FOLD_CH = 6;
  var LIST_MIN_CH = 100;

  // layout decides the window at width ch: {split, fold}. The split
  // outranks the tube's labels: it folds the tube before it gives up the
  // split. A single pane folds it when the list would be narrower than
  // LIST_MIN_CH. So, open widths: split from SPLIT_CH + TUBE_CH (158), split
  // folded from SPLIT_CH + TUBE_FOLD_CH (146), one pane from LIST_MIN_CH +
  // TUBE_CH (118), folded below that. It depends on the width alone, never
  // on whether a thread is open, so opening one never moves the tube.
  function layout(ch) {
    if (ch - TUBE_CH >= SPLIT_CH) return { split: true, fold: false };
    if (ch - TUBE_FOLD_CH >= SPLIT_CH) return { split: true, fold: true };
    return { split: false, fold: ch - TUBE_CH < LIST_MIN_CH };
  }

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
    archive: 'Archived', trash: 'Moved to trash', spam: 'Marked as spam', star: 'Starred',
    unstar: 'Unstarred', unread: 'Marked unread', read: 'Marked read',
  };

  function label(action) { return LABELS[action] || action; }

  var VERBS = {
    archive: 'Archive', trash: 'Trash', spam: 'Mark as spam', star: 'Star', unstar: 'Unstar', unread: 'Mark unread', read: 'Mark read',
  };

  function verb(action) { return VERBS[action] || action; }

  // ---- view generation (docs/client.md "Changes from other windows") -----
  // A label is {epoch, gen}: where a pane's render, a write or an event sits
  // in the server's view generation (view.go).

  // viewLabel reads one from a page's data-epoch/data-gen or an event; null
  // if it isn't one.
  function viewLabel(epoch, gen) {
    var g = typeof gen === 'number' ? gen : parseInt(gen, 10);
    if (typeof epoch !== 'string' || !epoch || !(g >= 0)) return null;
    return { epoch: epoch, gen: g };
  }

  // behind says a pane showing label must be fetched again to be at least
  // need (a hello, or an event that touched it). need is always of the
  // current epoch, so a label of another epoch is behind it.
  function behind(label, need) {
    if (!need) return false;
    if (!label || label.epoch !== need.epoch) return true;
    return label.gen < need.gen;
  }

  // advance: a pane at ev's previous gen that applied ev itself (its own
  // write) is now at ev's.
  function advance(label, ev) {
    if (label && label.epoch === ev.epoch && label.gen === ev.gen - 1) return { epoch: ev.epoch, gen: ev.gen };
    return label;
  }

  // names says ev touches ref ({account, thread}); an event without
  // threads (a sync) may have touched anything.
  function names(ev, ref) {
    if (!ref) return false;
    var ts = ev && Array.isArray(ev.threads) ? ev.threads : [];
    if (!ts.length) return true;
    return ts.some(function (t) { return !!t && t.account === ref.account && t.thread === ref.thread; });
  }

  function appliedKey(epoch, gen) { return epoch + ' ' + gen; }

  // viewDecision is what a `view` event does to this window. w: id (this
  // page load's X-Pneu-Window), applied (appliedKey -> true for each write
  // whose successful response this page applied), pending (its writes in
  // flight), open ({account, thread} the thread pane shows or is opening,
  // or null). `from` only ever saves a refresh: the event is skipped only
  // if it is this window's own write, already applied. While writes are in
  // flight it is held and decided again once they settle (pending 0), so a
  // failed or lost write reconciles. Anything else reconciles: the list
  // reloads, and the open thread too if the event names it.
  // {act: 'skip'|'hold'|'reload', thread}.
  function viewDecision(ev, w) {
    if (ev.from && ev.from === w.id) {
      if (w.applied && w.applied[appliedKey(ev.epoch, ev.gen)]) return { act: 'skip', thread: false };
      if (w.pending > 0) return { act: 'hold', thread: false };
    }
    return { act: 'reload', thread: names(ev, w.open) };
  }

  // undoConflicts marks the undo entries whose thread another window (or
  // a send) wrote after them: z still undoes them, last writer wins, but
  // the toast says so. An entry of another epoch predates a restart and
  // can't be compared. Pure: a new stack.
  function undoConflicts(stack, ev, id) {
    stack = Array.isArray(stack) ? stack : [];
    if (!ev || ev.from === id || !Array.isArray(ev.threads) || !ev.threads.length) return stack;
    return stack.map(function (e) {
      if (e.conflict || e.epoch !== ev.epoch || !(ev.gen > e.gen) || !names(ev, e)) return e;
      var c = {};
      Object.keys(e).forEach(function (k) { c[k] = e[k]; });
      c.conflict = true;
      return c;
    });
  }

  var T = {
    UNDO_CAP: UNDO_CAP, MAX_IDS: MAX_IDS, body: body, removeArgs: removeArgs, splitIds: splitIds, decodeId: decodeId,
    nextIndex: nextIndex, push: push, pop: pop, parse: parse, removes: removes, skip: skip,
    retryAfter: retryAfter, label: label, verb: verb,
    SPLIT_CH: SPLIT_CH, TUBE_CH: TUBE_CH, TUBE_FOLD_CH: TUBE_FOLD_CH, LIST_MIN_CH: LIST_MIN_CH, layout: layout, pathKind: pathKind, listURL: listURL, historyOp: historyOp, restoreIndex: restoreIndex,
    paneView: paneView, refreshStale: refreshStale, rgbHex: rgbHex, cycle: cycle,
    viewLabel: viewLabel, behind: behind, advance: advance, names: names, appliedKey: appliedKey,
    viewDecision: viewDecision, undoConflicts: undoConflicts,
  };
  if (typeof module === 'object' && module.exports) module.exports = T;
  else (root.Pneu = root.Pneu || {}).triage = T;
})(this);
