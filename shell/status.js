// status.js: the bar widget's decisions, as plain functions (no Qt), so
// node --test shell/status.test.js runs them (BarWidget.qml imports this
// file and draws what it says).
//
// status.json is the only input. Version 1 is a server's; version 2 a
// client's (docs/client.md, "Status file and the bar"), whose counts come
// from the server and whose `server` block says, in local codes, how the
// link to it is. `updated` and `running` are always this machine's daemon.
//
// Every string from the file is shown as text (the widget's Text items and
// the bar's tooltip are Text.PlainText), through clean() first. None ever
// reaches a command: the menu's commands are the fixed argv in ITEMS, and
// an item is chosen by state, never built from the file.

// Well past the daemon's 5-minute heartbeat, and past the 15-minute cap on
// sync backoff, so a slow week of failures still reads as failures, not as
// a dead daemon. A client's counts go stale on the same clock, by
// server.statusAt.
var STALE_AFTER = 20 * 60 * 1000

// How long a client's link may say `starting` before the widget calls it
// unreachable: a first attempt ends well inside it.
var CONNECT_GRACE = 90 * 1000

// How long Reopen pneu stays in the menu after Reset window data.
var REOPEN_FOR = 30 * 60 * 1000

// isLink: one of server.link's three values. Compared, never looked up:
// an object used as a set answers "__proto__" and "toString" too.
function isLink(v) {
  return v === "starting" || v === "up" || v === "down"
}

// own: a lookup in one of this file's tables only for its own keys.
function own(table, key) {
  return typeof key === "string" && Object.prototype.hasOwnProperty.call(table, key)
}

// The link's reason codes (internal/link), in the widget's words. Local
// codes: a reason not here is shown as "can't reach".
var REASONS = {
  "tailscale-down": "Tailscale is off on this machine",
  "node-offline": "{s} is offline",
  "node-mismatch": "{s}'s address belongs to another machine",
  "refused": "{s} is up but pneu isn't answering",
  "pin-mismatch": "{s}'s identity changed; not connecting",
  "not-paired": "{s} doesn't know this machine",
  "protocol": "this machine and {s} run different versions"
}

// isUpdate: one of server.update.state's three values, compared.
function isUpdate(v) {
  return v === "client-older" || v === "server-older" || v === "different"
}

// The menu: every command is a fixed argv, run without a shell. Nothing
// from status.json is ever put into one. Update pneu runs `pneu update` in
// Omarchy's floating terminal (it asks before changing anything; the
// launcher joins its arguments into a bash line, so they're fixed words);
// Update <server> asks the agent, whose prompt says how to update the
// server from its own checkout (docs/client.md "Versions and updates").
var ITEMS = {
  open: { label: "Open pneu", argv: ["pneu", "open"] },
  agent: { label: "Fix with agent", argv: ["pneu", "agent"] },
  update: { label: "Update pneu", argv: ["omarchy-launch-floating-terminal-with-presentation", "pneu", "update"] },
  updateServer: { label: "Update the server", argv: ["pneu", "agent", "-update"] },
  reopen: { label: "Reopen pneu (reset done)", argv: ["pneu", "open"] },
  reset: { label: "Reset window data…", argv: ["pneu", "reset-window"] }
}

// What Reset window data asks before it runs.
var RESET_CONFIRM = "Closes pneu's app windows and clears its site data in the browser, which deletes compose drafts saved there. If pneu is also open in a browser tab or popup, quit the browser first: the reset can't close those. You'll be asked for one click in the browser's settings."

// clean is a string from the file as the widget shows it: control,
// format (bidi) and line/paragraph separator characters dropped, at most
// n characters (default 128).
function clean(s, n) {
  var out = String(s === null || s === undefined ? "" : s)
    .replace(/[\u0000-\u001f\u007f-\u009f\u00ad\u061c\u180e\u200b-\u200f\u2028-\u202e\u2060-\u206f\ufeff\ufff9-\ufffb]/g, "")
  var max = n || 128
  var chars = Array.from(out)
  return chars.length > max ? chars.slice(0, max).join("") + "…" : out
}

function time(s) {
  return s === null || s === undefined ? NaN : Date.parse(String(s))
}

// parse is status.json's text as a doc, or null: version 1, or version 2
// with a well-formed server block.
function parse(raw) {
  var doc = null
  try { doc = JSON.parse(raw) } catch (e) { return null }
  if (doc === null || typeof doc !== "object" || Array.isArray(doc)) return null
  if (doc.version === 1) return doc
  if (doc.version !== 2) return null
  var s = doc.server
  if (s === null || typeof s !== "object" || Array.isArray(s)) return null
  if (!isLink(s.link)) return null
  if (typeof s.name !== "string") return null
  return doc
}

function accountSick(a) {
  if (!a) return false
  // A pneu too old to write state: not pulled was the warning.
  if (a.state === undefined) return (parseInt(a.failures) || 0) > 0 || !!a.error || a.pulled === false
  if (a.state === "reauth" || a.state === "unconfigured" || a.state === "unauthorized") return true
  return (parseInt(a.failures) || 0) > 0 || (a.error !== null && a.error !== undefined && a.error !== "")
}

// model is everything the widget draws, from the parsed doc (or null),
// whether the file was missing, and the clock.
function model(doc, missing, now) {
  var m = {
    missing: !!missing, unreadable: doc === null, version: doc ? doc.version : 0,
    accounts: [], unread: 0, senders: [], updatedAt: NaN, stale: true, stopped: false,
    sick: [], pulling: [], pullPercent: -1,
    server: null, link: "", connecting: false, linkDown: false, countsStale: false, statusAt: NaN, linkSince: NaN,
    update: ""
  }
  if (!doc) { m.warning = true; return m }
  m.accounts = Array.isArray(doc.accounts) ? doc.accounts.filter(function (a) { return a && typeof a === "object" }) : []
  m.unread = parseInt(doc.unread) || 0
  m.senders = Array.isArray(doc.senders) ? doc.senders.map(function (x) { return clean(x, 64) }) : []
  m.updatedAt = time(doc.updated || "")
  m.stale = !isFinite(m.updatedAt) || now - m.updatedAt > STALE_AFTER
  m.stopped = doc.running !== true
  m.sick = m.accounts.filter(accountSick)
  // Accounts before or in their first pull, and the least-done one's
  // percent: -1 while any has none yet (waiting, or still listing).
  m.pulling = m.accounts.filter(function (a) { return a.state === "pulling" || a.state === "needs-pull" })
  var pct = 100
  for (var i = 0; i < m.pulling.length; i++) {
    var p = m.pulling[i].progress
    var v = p && p.percent !== null && p.percent !== undefined ? parseInt(p.percent) : NaN
    if (isNaN(v)) { pct = -1; break }
    pct = Math.min(pct, v)
  }
  m.pullPercent = m.pulling.length > 0 ? pct : -1

  if (doc.version === 2) {
    var s = doc.server
    m.server = clean(s.name, 64) || "the server"
    m.link = s.link
    m.reason = typeof s.reason === "string" ? s.reason : ""
    m.linkSince = time(s.linkSince)
    m.statusAt = time(s.statusAt)
    // starting is not an error for its first CONNECT_GRACE.
    var young = isFinite(m.linkSince) && now - m.linkSince < CONNECT_GRACE
    m.connecting = m.link === "starting" && young
    m.linkDown = m.link === "down" || (m.link === "starting" && !young)
    m.countsStale = m.link !== "up" || !isFinite(m.statusAt) || now - m.statusAt > STALE_AFTER
    // The version nudge: its state only, compared; anything else is none.
    var u = s.update
    if (u !== null && typeof u === "object" && !Array.isArray(u) && isUpdate(u.state)) m.update = u.state
  }
  // The counts can't be trusted: no file, this daemon stopped or silent,
  // an account failing, or (a client) the server out of reach or silent.
  // Connecting is neutral: it says so, but isn't a warning yet.
  m.warning = m.stopped || m.stale || m.sick.length > 0 ||
    (m.version === 2 && !m.connecting && m.countsStale)
  return m
}

// clock words a time as HH:mm; fmt.clock is Qt's in the widget.
function clockOf(ms, fmt) {
  return isFinite(ms) ? fmt.clock(ms) : "never"
}

function num(n) {
  var s = String(parseInt(n) || 0)
  for (var i = s.length - 3; i > 0; i -= 3) s = s.slice(0, i) + "," + s.slice(i)
  return s
}

// pullLine words a first pull as app.js and `pneu account status` do.
function pullLine(a, fmt) {
  var name = clean(a.name || "?", 64)
  var p = a.progress
  if (a.state !== "pulling" || !p) return name + ": waiting to download"
  var out
  if (p.phase === "listing") out = "listing messages: " + num(p.done) + " found"
  else if (p.phase === "content") out = "downloading " + num(p.done) + " of " + num(p.total)
  else if (p.phase === "metadata") out = "checking labels " + num(p.done) + " of " + num(p.total)
  else if (p.phase === "removing") out = "removing deleted messages"
  else out = "starting the download"
  if (p.percent !== null && p.percent !== undefined) out += " (" + (parseInt(p.percent) || 0) + "%)"
  if (p.frontier) {
    var t = Date.parse(String(p.frontier))
    if (isFinite(t)) out += ", complete back to " + fmt.day(t)
  }
  return name + ": " + out
}

// NAME_RE is config.ValidName's rule; word is a name as a suggested
// command shows it, the placeholder <account> if it ever fails the rule.
var NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$/

function word(name) {
  return typeof name === "string" && NAME_RE.test(name) ? name : "<account>"
}

// accountLine words a failing account. On a client the fix runs on the
// server, so its commands say where.
function accountLine(a, m) {
  var name = clean(a.name || "?", 64)
  var where = m && m.version === 2 ? " on " + m.server : ""
  if (a.state === undefined && a.pulled === false) return name + ": first pull not finished"
  // A client's page can't run the consent (409 reauth-on-server);
  // pneu account auth here forwards it to the server.
  if (a.state === "reauth" && where) return name + ": Gmail access expired or was revoked (pneu account auth " + word(a.name) + " here, or Fix with agent)"
  if (a.state === "reauth") return name + ": Gmail access expired or was revoked; reconnect in pneu"
  if (a.state === "unconfigured") return name + ": not set up (pneu account add " + word(a.name) + " <address>" + where + ")"
  if (a.state === "unauthorized") return name + ": not connected to Gmail (pneu account auth " + word(a.name) + where + ")"
  var failures = parseInt(a.failures) || 0
  var err = a.error ? clean(a.error) : ""
  var head = name + (failures > 0 ? ": " + failures + " failed sync" + (failures === 1 ? "" : "s") : ": error")
  return err ? head + " · " + err : head
}

function unreadLine(m) {
  if (m.unread === 0) return "No unread mail"
  var names = m.senders.slice(0, Math.min(m.unread, 3))
  if (names.length === 0) return m.unread + " unread"
  var extra = m.unread - names.length
  return m.unread + " unread · " + names.join(", ") + (extra > 0 ? " +" + extra : "")
}

// linkLine is a client's link, when there is something to say about it.
function linkLine(m, fmt) {
  if (m.version !== 2) return ""
  if (m.connecting) return "Connecting to " + m.server + "…"
  if (m.linkDown) {
    var why = own(REASONS, m.reason) ? " · " + REASONS[m.reason].replace("{s}", m.server) : ""
    return "Can't reach " + m.server + " since " + clockOf(m.linkSince, fmt) + why
  }
  if (m.countsStale) return "No word from " + m.server + " since " + clockOf(m.statusAt, fmt)
  return ""
}

// tooltip is the widget's tooltip text. Version 1 reads exactly as it
// always has.
function tooltip(m, fmt) {
  if (m.missing) return "pneu: no status yet. Is the server installed and running?\nsystemctl --user status pneu"
  if (m.unreadable) return "pneu: status.json unreadable (a newer or older pneu?)"
  var lines = []
  var self = m.version === 2 ? "pneu" : "pneu server"
  if (m.stopped) lines.push(self + " stopped (last update " + clockOf(m.updatedAt, fmt) + ")")
  else if (m.stale) lines.push(self + " not responding (last update " + clockOf(m.updatedAt, fmt) + ")")
  var ll = linkLine(m, fmt)
  if (ll) lines.push(ll)
  var ul = unreadLine(m)
  if (m.version === 2 && m.countsStale && !m.connecting) ul += " (as of " + clockOf(m.statusAt, fmt) + ")"
  lines.push(ul)
  for (var j = 0; j < m.pulling.length; j++) lines.push(pullLine(m.pulling[j], fmt))
  for (var i = 0; i < m.sick.length; i++) lines.push(accountLine(m.sick[i], m))
  if (m.version === 2 && menu(m, false).some(function (it) { return it.id === "agent" })) lines.push("Right-click: Fix with agent")
  var nudge = updateLine(m)
  if (nudge) lines.push(nudge)
  return lines.join("\n")
}

// updateLine is the version nudge's tooltip line, "" without one.
function updateLine(m) {
  if (m.version !== 2) return ""
  if (m.update === "client-older") return "Update available: " + m.server + " runs a newer pneu · right-click: Update pneu"
  if (m.update === "server-older") return m.server + " runs an older pneu · right-click: Update " + m.server
  if (m.update === "different") return "This machine and " + m.server + " run different pneu builds · right-click: Update pneu"
  return ""
}

// menu is the click menu's items, in order: [{id, label}]. Fix with
// agent only when there's something for it (a client's link out, a
// protocol mismatch, its daemon gone, an account failing); Reopen pneu
// only after a reset this widget started (resetPending: the widget's own
// state, not the file's).
function menu(m, resetPending) {
  var ids = ["open"]
  var agent = m.sick.length > 0
  if (m.version === 2) agent = agent || m.linkDown || m.reason === "protocol" || m.stopped || m.stale ||
    (m.link === "up" && m.countsStale)
  if (agent) ids.push("agent")
  if (m.version === 2 && (m.update === "client-older" || m.update === "different")) ids.push("update")
  if (m.version === 2 && m.update === "server-older") ids.push("updateServer")
  if (resetPending) ids.push("reopen")
  ids.push("reset")
  return ids.map(function (id) {
    // The one label with a name in it: the server's, cleaned, drawn as
    // plain text. The command is still the item's fixed argv.
    return { id: id, label: id === "updateServer" ? "Update " + m.server : ITEMS[id].label }
  })
}

// argv is an item's command, a fresh copy of its fixed argv; null for an
// unknown id.
function argv(id) {
  if (!own(ITEMS, id)) return null
  return ITEMS[id].argv.slice()
}

if (typeof module === "object" && module && module.exports) {
  module.exports = {
    STALE_AFTER: STALE_AFTER, CONNECT_GRACE: CONNECT_GRACE, REOPEN_FOR: REOPEN_FOR, ITEMS: ITEMS, RESET_CONFIRM: RESET_CONFIRM,
    clean: clean, word: word, parse: parse, model: model, accountSick: accountSick, tooltip: tooltip, menu: menu, argv: argv,
    pullLine: pullLine, accountLine: accountLine, unreadLine: unreadLine, linkLine: linkLine, updateLine: updateLine
  }
}
