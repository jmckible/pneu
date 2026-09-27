import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

// pneu's bar presence: unread inbox threads across every account, and a
// warning when the mail isn't flowing.
//
// Everything comes from ~/.local/state/pneu/status.json, which the server
// rewrites at startup, after every sync and push, shortly after triage, on a
// five-minute heartbeat, and with running:false on a clean stop. So the
// widget runs nothing and polls nothing; it works with the window parked or
// closed, and it never touches notmuch or the install token.
//
// Warning (the mark in the urgent colour) means the count can't be trusted:
// no readable file, the server stopped, the file older than staleAfter
// (so the server is gone or wedged whatever sync is doing), or an account
// that is failing, erroring, needs re-auth, or isn't set up. An account
// downloading its mail for the first time isn't a warning: the count shows
// its progress ("43%") instead, and the tooltip how far back it's complete.
BarWidget {
  id: root
  moduleName: "pneu"

  // Well past the server's heartbeat, and past the 15-minute cap on sync
  // backoff, so a slow week of failures still reads as failures, not as a
  // dead server.
  readonly property int staleAfter: 20 * 60 * 1000

  property var status: null     // parsed status.json, or null
  property bool unreadable: true // missing or unparseable
  property bool missing: true
  property double now: Date.now()

  readonly property var accounts: status && Array.isArray(status.accounts) ? status.accounts : []
  readonly property int unread: status ? (parseInt(status.unread) || 0) : 0
  readonly property var senders: status && Array.isArray(status.senders) ? status.senders.map(String) : []
  readonly property double updatedAt: status ? Date.parse(String(status.updated || "")) : NaN
  readonly property bool stale: !isFinite(updatedAt) || now - updatedAt > staleAfter
  readonly property bool stopped: status !== null && status.running !== true
  readonly property var sickAccounts: accounts.filter(function (a) { return root.accountSick(a) })
  readonly property bool warning: unreadable || stopped || stale || sickAccounts.length > 0
  // Accounts before or in their first pull, and the least-done one's
  // percent: -1 while any has none yet (waiting, or still listing).
  readonly property var pullingAccounts: accounts.filter(function (a) { return a && (a.state === "pulling" || a.state === "needs-pull") })
  readonly property int pullPercent: {
    var pct = 100
    for (var i = 0; i < pullingAccounts.length; i++) {
      var p = pullingAccounts[i].progress
      var v = p && p.percent !== null && p.percent !== undefined ? parseInt(p.percent) : NaN
      if (isNaN(v)) return -1
      pct = Math.min(pct, v)
    }
    return pullingAccounts.length > 0 ? pct : -1
  }

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  function accountSick(a) {
    if (!a) return false
    // A pneu too old to write state: not pulled was the warning.
    if (a.state === undefined) return (parseInt(a.failures) || 0) > 0 || !!a.error || a.pulled === false
    if (a.state === "reauth" || a.state === "unconfigured" || a.state === "unauthorized") return true
    return (parseInt(a.failures) || 0) > 0 || (a.error !== null && a.error !== undefined && a.error !== "")
  }

  // The server replaces the file by rename; watchChanges follows that and
  // the file's first creation. text() is stale inside the change signal, so
  // every read goes through reload() -> onLoaded.
  FileView {
    id: file
    path: Quickshell.env("HOME") + "/.local/state/pneu/status.json"
    watchChanges: true
    printErrors: false
    onLoaded: root.apply(text(), false)
    onLoadFailed: root.apply("", true)
    onFileChanged: reload()
  }

  function apply(raw, missing) {
    var parsed = null
    try { parsed = JSON.parse(raw) } catch (e) {}
    var ok = parsed !== null && typeof parsed === "object" && parsed.version === 1
    root.status = ok ? parsed : null
    root.unreadable = !ok
    root.missing = missing
    root.now = Date.now()
  }

  // Staleness is a function of the clock, not of the file, so the clock is
  // what ticks. A minute is fine resolution against a 20-minute threshold.
  Timer {
    interval: 60000
    running: true
    repeat: true
    onTriggered: root.now = Date.now()
  }

  function clockOf(ms) {
    return isFinite(ms) ? Qt.formatDateTime(new Date(ms), "HH:mm") : "never"
  }

  function num(n) {
    var s = String(parseInt(n) || 0)
    for (var i = s.length - 3; i > 0; i -= 3) s = s.slice(0, i) + "," + s.slice(i)
    return s
  }

  // pullLine words a first pull as app.js and `pneu account status` do.
  function pullLine(a) {
    var name = String(a.name || "?")
    var p = a.progress
    if (a.state !== "pulling" || !p) return name + ": waiting to download"
    var out
    if (p.phase === "listing") out = "listing messages: " + num(p.done) + " found"
    else if (p.phase === "content") out = "downloading " + num(p.done) + " of " + num(p.total)
    else if (p.phase === "metadata") out = "checking labels " + num(p.done) + " of " + num(p.total)
    else if (p.phase === "removing") out = "removing deleted messages"
    else out = "starting the download"
    if (p.percent !== null && p.percent !== undefined) out += " (" + p.percent + "%)"
    if (p.frontier) {
      var t = Date.parse(String(p.frontier))
      if (isFinite(t)) out += ", complete back to " + Qt.formatDateTime(new Date(t), "d MMM yyyy")
    }
    return name + ": " + out
  }

  function accountLine(a) {
    var name = String(a.name || "?")
    if (a.state === undefined && a.pulled === false) return name + ": first pull not finished"
    if (a.state === "reauth") return name + ": Gmail access expired or was revoked; reconnect in pneu"
    if (a.state === "unconfigured") return name + ": not set up (pneu account add " + name + " <address>)"
    if (a.state === "unauthorized") return name + ": not connected to Gmail (pneu account auth " + name + ")"
    var failures = parseInt(a.failures) || 0
    var err = a.error ? String(a.error) : ""
    var head = name + (failures > 0 ? ": " + failures + " failed sync" + (failures === 1 ? "" : "s") : ": error")
    return err ? head + " · " + err : head
  }

  function unreadLine() {
    if (root.unread === 0) return "No unread mail"
    var names = root.senders.slice(0, Math.min(root.unread, 3))
    if (names.length === 0) return root.unread + " unread"
    var extra = root.unread - names.length
    return root.unread + " unread · " + names.join(", ") + (extra > 0 ? " +" + extra : "")
  }

  readonly property string tooltip: {
    if (root.missing) return "pneu: no status yet. Is the server installed and running?\nsystemctl --user status pneu"
    if (root.unreadable) return "pneu: status.json unreadable (a newer or older pneu?)"
    var lines = []
    if (root.stopped) lines.push("pneu server stopped (last update " + root.clockOf(root.updatedAt) + ")")
    else if (root.stale) lines.push("pneu server not responding (last update " + root.clockOf(root.updatedAt) + ")")
    lines.push(root.unreadLine())
    for (var j = 0; j < root.pullingAccounts.length; j++) lines.push(root.pullLine(root.pullingAccounts[j]))
    for (var i = 0; i < root.sickAccounts.length; i++) lines.push(root.accountLine(root.sickAccounts[i]))
    return lines.join("\n")
  }

  function open() {
    if (!root.bar) return
    var command = String(root.setting("command", "")).trim()
    // bar.run is `bash -lc`, and Omarchy's login profile puts ~/.local/bin
    // on PATH, so the bare name finds a ~/.local/bin install.
    root.bar.run(command !== "" ? command : "pneu open")
  }

  FontLoader {
    id: markFont
    source: Qt.resolvedUrl("../brand/pneu-mark.ttf")
  }

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar

    // Colour carries the state: bar foreground at rest, accent with unread
    // mail, urgent when the count can't be trusted. Theme tokens only, via
    // `bar.*` where the bar offers one so shell.toml overrides reach it.
    readonly property color stateColor: root.warning
      ? (root.bar ? root.bar.urgent : Color.urgent)
      : (root.unread > 0 ? Color.accent : (root.bar ? root.bar.barForeground : Color.foreground))
    // A first download shows its progress in place of the count.
    readonly property bool pulling: !root.warning && root.pullingAccounts.length > 0
    readonly property bool showCount: pulling || root.unread > 0
    readonly property string countText: pulling
      ? (root.pullPercent >= 0 ? root.pullPercent + "%" : "…")
      : (root.unread > 99 ? "99+" : String(root.unread))
    // Arithmetic, not measured: the labels live inside a Component. The
    // canvas must fit glyph and count or the glyph is clipped away.
    readonly property int countSize: showCount
      ? (root.vertical ? Style.font.caption : countText.length * Style.space(6)) + Style.space(3) : 0
    readonly property int contentSize: Style.bar.iconFont + countSize

    slotSize: contentSize + Style.space(10)
    opticalSize: contentSize
    tooltipText: root.tooltip
    onPressed: function (b) { if (b === Qt.LeftButton) root.open() }

    // NativeRendering: the default distance-field rasteriser blurs a 10px
    // digit at bar scale.
    iconComponent: Component {
      Item {
        Grid {
          anchors.centerIn: parent
          columns: root.vertical ? 1 : 2
          spacing: Style.space(3)
          horizontalItemAlignment: Grid.AlignHCenter
          verticalItemAlignment: Grid.AlignVCenter

          // pneu's mark (brand/README.md), a hinted glyph like the bar's own
          // icons, in the state colour.
          Text {
            text: "\ue000"
            textFormat: Text.PlainText
            color: button.stateColor
            font.family: markFont.name
            font.pixelSize: Style.bar.iconFont
            renderType: Text.NativeRendering
          }

          Text {
            visible: button.showCount
            text: button.countText
            textFormat: Text.PlainText
            color: button.stateColor
            font.bold: true
            font.family: button.fontFamily
            font.pixelSize: Style.font.caption
            renderType: Text.NativeRendering
          }
        }
      }
    }
  }
}
