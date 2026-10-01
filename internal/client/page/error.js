// The client's error pages (internal/client/errorpage.go). Nothing here
// comes from the server.
//
// The link page reloads once the link is up, which this daemon's own
// /events says (its hello, then `link`). Its button asks for an attempt
// now, whose success reloads it; with the link up already (the load
// failed on a connection, not the link) it reloads at once. The send page (a compose form's POST /send the link failed)
// never reloads, which would post the form again: its button goes back to
// the draft, which compose.js kept.
(function () {
  'use strict';
  var back = document.getElementById('back');
  if (document.body.dataset.kind === 'send') {
    if (back) back.addEventListener('click', function () {
      if (history.length > 1) history.back();
      else location.replace('/compose');
    });
    return;
  }
  var last = null; // the link as last heard
  function up(l) { return !!l && l.state === 'up'; }
  function reloadIf(l, after) { if (up(l)) setTimeout(function () { location.reload(); }, after); }
  // upDelay: up already when this page subscribed, so it was served on a
  // load that failed as the link went down (not marked yet) or on a
  // connection that failed with the link up. Each such reload in a row
  // waits twice as long, 1s to 30s, so a server failing every load isn't
  // reloaded in a loop; a minute without one starts again at 1s. The
  // count lives in sessionStorage, which may be refused: then it's 1s.
  var KEY = 'pneu-error-reloads';
  function upDelay() {
    var n = 0, now = Date.now();
    try {
      var r = JSON.parse(sessionStorage.getItem(KEY) || 'null');
      if (r && typeof r.n === 'number' && typeof r.at === 'number' && now - r.at < 60000) n = Math.min(r.n, 5);
      sessionStorage.setItem(KEY, JSON.stringify({ n: n + 1, at: now }));
    } catch (err) { /* ignore */ }
    return 1000 * Math.pow(2, n);
  }
  if (window.EventSource) {
    var es = new EventSource('/events');
    // moved: still down, but for another reason than this page says (a
    // wake page whose window ran out on a real failure, or the reverse):
    // a reload shows the page for it.
    var shown = document.body.dataset.reason;
    function moved(l) { return !!l && !up(l) && typeof l.reason === 'string' && l.reason !== shown; }
    es.addEventListener('hello', function (e) {
      try {
        last = JSON.parse(e.data).link;
        if (up(last)) reloadIf(last, upDelay());
        else if (moved(last)) location.reload();
      } catch (err) { /* ignore */ }
    });
    es.addEventListener('link', function (e) {
      try {
        last = JSON.parse(e.data);
        if (up(last)) reloadIf(last, 0);
        else if (moved(last)) location.reload();
      } catch (err) { /* ignore */ }
    });
  }
  var btn = document.getElementById('retry');
  if (btn) btn.addEventListener('click', function () {
    if (up(last)) { location.reload(); return; }
    btn.disabled = true;
    fetch('/client/retry', { method: 'POST', credentials: 'same-origin' })
      .catch(function () {})
      .then(function () { setTimeout(function () { btn.disabled = false; }, 2000); });
  });
})();
