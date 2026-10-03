// push.js — the pure half of the sync details' instant mail lines and
// polling note (docs/push.md D7): an account's push health in words, the
// command that fixes it, and the poll delay as the engine has it. No DOM;
// app.js builds the nodes with textContent and web/push.test.js runs
// these under node --test. Nothing here touches the status line: push
// never turns anything red, and only R shows Checking….
(function (root) {
  'use strict';

  function own(o, k) { return Object.prototype.hasOwnProperty.call(o, k); }

  // The push manager's closed vocabulary (control.PushStates and the two
  // reason lists); absent is off. A reason's fix: run (the account's
  // `pneu account push`), init (`pneu push init --reconsent`), explain
  // (`pneu account push` prints the fix in full), or none.
  var PLAIN = { starting: 'starting', listening: 'listening · no message since pneu started', quiet: 'quiet · nothing in 24h' };
  var REAUTH = {
    'owner-reauth': { words: "the push owner's Google grant expired or was revoked", fix: 'init' },
    'mailbox-reauth': { words: "this mailbox's grant for instant mail expired or was revoked", fix: 'run' },
  };
  var FAILING = {
    'api-disabled': { words: "an API isn't enabled in the push project", fix: 'explain' },
    permission: { words: 'the push owner lacks permission in the push project', fix: 'explain' },
    'org-policy': { words: 'an organization policy refused it', fix: 'explain' },
    network: { words: "can't reach Google; it keeps trying", fix: 'none' },
    'watch-expired': { words: "Gmail's watch lapsed", fix: 'run' },
    unknown: { words: 'something went wrong', fix: 'run' },
  };
  var DELIVERY_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/;

  // line is an account's instant mail line: { text, fix }, fix null or
  // { lead, command, tail } (the command shown as code). push is the
  // account view's push (absent: off). o: word (the account name as a
  // command shows it), server (the server's name on a client, else
  // null), now (ms), ago (app.js's age words).
  function line(push, o) {
    var p = push && typeof push === 'object' ? push : {};
    var st = typeof p.state === 'string' ? p.state : '';
    if (st === 'delivering') {
      var t = typeof p.lastDelivery === 'string' && DELIVERY_RE.test(p.lastDelivery) ? Date.parse(p.lastDelivery) : NaN;
      return { text: 'Instant: delivering' + (isNaN(t) ? '' : ' · last message ' + o.ago(Math.max(0, o.now - t))), fix: null };
    }
    if (own(PLAIN, st)) return { text: 'Instant: ' + PLAIN[st], fix: null };
    var table = st === 'reauth' ? REAUTH : st === 'failing' ? FAILING : null;
    if (!table) return { text: 'Instant: off', fix: null };
    var r = typeof p.reason === 'string' && own(table, p.reason) ? table[p.reason] : FAILING.unknown;
    return { text: 'Instant: failing — ' + r.words, fix: fix(r.fix, o) };
  }

  // fix words a reason's fix. On a client the command runs in a terminal
  // there, over SSH on the server, consent opening in this browser, as
  // `pneu account auth` does (link.js reauthHelp).
  function fix(kind, o) {
    if (kind === 'none') return null;
    var command = kind === 'init' ? 'pneu push init --reconsent' : 'pneu account push ' + o.word;
    var where = o.server ? ' in a terminal on this machine (it runs on ' + o.server + ' over SSH and opens Google here if it needs to)' : '';
    if (kind === 'explain') {
      return { lead: '', command: command, tail: ' prints the fix' + (o.server ? '; run it' + where : '') + '.' };
    }
    return { lead: 'Run ', command: command, tail: where + '.' };
  }

  // seconds is an account view's pollEvery, or 0 when it has none.
  function seconds(a) {
    var n = a && a.pollEvery;
    return typeof n === 'number' && n % 1 === 0 && n >= 1 && n <= 86400 ? n : 0;
  }

  // every words a delay in seconds: 30s, 4m, 2h.
  function every(s) {
    if (s < 60) return s + 's';
    if (s < 3600) return Math.round(s / 60) + 'm';
    return Math.round(s / 3600) + 'h';
  }

  // poll is the polling note: { foot, per }. foot is the details' footer,
  // the shortest delay among the ready accounts (others are waiting on
  // setup and look again sooner), else fallback (the page's data-every,
  // in seconds); per maps each ready account polling slower than that
  // (backing off after failures) to its own note, with no prototype: an
  // account may be called constructor.
  function poll(accounts, fallback) {
    var ready = (accounts || []).filter(function (a) { return a && a.state === 'ready' && seconds(a); });
    var base = ready.length ? Math.min.apply(null, ready.map(seconds)) : fallback;
    var per = Object.create(null);
    ready.forEach(function (a) {
      if (seconds(a) > base) per[a.name] = 'checks every ' + every(seconds(a)) + ' while failing';
    });
    return { foot: 'Checks every ' + every(base), per: per };
  }

  var api = { line: line, poll: poll, every: every };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else (root.Pneu = root.Pneu || {}).push = api;
})(this);
