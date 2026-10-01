// link.js — the pure half of client mode's link in the page: the status
// line's link-down state, the toast a failed mutation's Pneu-Link outcome
// gets, and the details panel's rows (docs/client.md). No DOM, no fetch;
// app.js wires these up and web/link.test.js runs them under node --test.
// On a server there is no link (hello carries none), and every function
// here says nothing.
(function (root) {
  'use strict';

  // The daemon's link states and reason codes (internal/link): local codes,
  // never the server's words.
  var REASONS = {
    starting: 'connecting',
    'tailscale-down': 'Tailscale is off on this machine',
    'node-offline': '{s} is offline',
    'node-mismatch': "{s}'s address belongs to another machine",
    refused: "{s} is up but pneu isn't answering",
    'pin-mismatch': "{s}'s identity changed; not connecting",
    'not-paired': "{s} doesn't know this machine",
    protocol: 'this machine and {s} run different versions',
  };

  function serverName(link) {
    return link && typeof link.server === 'string' && link.server ? link.server : 'the server';
  }

  // valid: a link view as the daemon sends it (hello's link, SSE `link`).
  function valid(link) {
    return !!link && typeof link === 'object' && (link.state === 'up' || link.state === 'down' || link.state === 'starting');
  }

  // lineState is the status line's state while the link is down: the
  // line's highest priority. null when up, starting, or on a server.
  function lineState(link) {
    if (!valid(link) || link.state !== 'down') return null;
    return { state: 'link', text: "Can't reach " + serverName(link) + ' · retrying' };
  }

  // outcome is a failed answer's Pneu-Link (header, or the JSON's `link`):
  // 'not-sent', 'unknown', or '' (the server answered, or a server).
  function outcome(headers, data) {
    var v = headers && typeof headers.get === 'function' ? headers.get('Pneu-Link') : null;
    if (!v && data && typeof data === 'object') v = data.link;
    return v === 'not-sent' || v === 'unknown' ? v : '';
  }

  // failText is the toast for a mutation that didn't reach the server
  // (safe to press again) or whose outcome is unknown (the next hello's
  // generation shows the truth). null for anything else.
  function failText(out, link) {
    var s = serverName(link);
    if (out === 'not-sent') return 'Not sent: can\'t reach ' + s + '.';
    if (out === 'unknown') return s + " didn't answer; checking when it's back.";
    return null;
  }

  // reasonText words a reason code for the details.
  function reasonText(reason, link) {
    var t = Object.prototype.hasOwnProperty.call(REASONS, reason) ? REASONS[reason] : String(reason || 'unknown');
    return t.replace('{s}', serverName(link));
  }

  function rev(r) {
    return typeof r === 'string' && /^[0-9a-f]{40}$/.test(r) ? r.slice(0, 12) : 'unknown';
  }

  // details are the details panel's link rows, [label, text] each; none on
  // a server.
  function details(link) {
    if (!valid(link)) return [];
    var s = serverName(link);
    var state = link.state === 'up' ? 'connected' : link.state === 'starting' ? 'connecting' : reasonText(link.reason, link);
    var rows = [[s, state + (link.since ? ' since ' + link.since : '')]];
    var r = link.revision || {};
    rows.push(['this build', rev(r.client)]);
    rows.push([s + "'s build", rev(r.server)]);
    return rows;
  }

  var api = { lineState: lineState, outcome: outcome, failText: failText, reasonText: reasonText, details: details };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else (root.Pneu = root.Pneu || {}).link = api;
})(this);
