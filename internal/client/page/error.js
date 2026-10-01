// The client's error pages (internal/client/errorpage.go). Nothing here
// comes from the server.
//
// The link page reloads once the link is up, which this daemon's own
// /events says (its hello, then `link`), and asks for an attempt now on
// its button. The send page (a compose form's POST /send the link failed)
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
  function up(l) { return !!l && l.state === 'up'; }
  function reloadIf(l, after) { if (up(l)) setTimeout(function () { location.reload(); }, after); }
  if (window.EventSource) {
    var es = new EventSource('/events');
    // Up already when this page subscribed: it was served on a send that
    // failed as the link went down, which the link hasn't marked yet. A
    // second's wait keeps that from becoming a reload loop.
    es.addEventListener('hello', function (e) {
      try { reloadIf(JSON.parse(e.data).link, 1000); } catch (err) { /* ignore */ }
    });
    es.addEventListener('link', function (e) {
      try { reloadIf(JSON.parse(e.data), 0); } catch (err) { /* ignore */ }
    });
  }
  var btn = document.getElementById('retry');
  if (btn) btn.addEventListener('click', function () {
    btn.disabled = true;
    fetch('/client/retry', { method: 'POST', credentials: 'same-origin' })
      .catch(function () {})
      .then(function () { setTimeout(function () { btn.disabled = false; }, 2000); });
  });
})();
