import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Commons as Commons
import qs.Ui
import "status.js" as Status

// pneu's bar presence: unread inbox threads across every account, a
// warning when the mail isn't flowing, and a menu (right-click) for the
// actions that reach outside the browser.
//
// Everything comes from ~/.local/state/pneu/status.json, which this
// machine's pneu rewrites at startup, after every sync and push (on a
// client, whenever the server's status arrives or the link changes),
// shortly after triage, on a five-minute heartbeat, and with running:false
// on a clean stop. So the widget polls nothing; it works with the window
// parked or closed, and it never touches notmuch or the install token.
// The decisions are status.js's (node --test shell/status.test.js): this
// file draws them.
//
// Version 1 is a server's file, version 2 a client's (docs/client.md): its
// counts come from the server, and server.link and server.statusAt say
// whether they're current. Warning (the mark in the urgent colour) means
// the count can't be trusted: no readable file, this machine's pneu
// stopped or silent past staleAfter, an account failing, erroring, needing
// re-auth or not set up, or (a client) the server out of reach or silent.
// A client still connecting just after start isn't a warning, and neither
// is an account downloading its mail for the first time: the count shows
// its progress ("43%") instead, and the tooltip how far back it's complete.
//
// A client's server.update (the version nudge) adds Update pneu or Update
// <server> to the menu; it's never a warning.
//
// Every string from the file is drawn as Text.PlainText (this file's Text
// items, the shell's Buttons, the bar's tooltip), after status.js's clean().
// None reaches a command: the menu runs only status.js's fixed argv, chosen
// by item id, through Util.execArgv (no shell).
//
// The Minimal style is for bars that keep only what needs attention: the
// mark alone in the accent colour while there is unread mail, nothing
// otherwise. A warning still shows (in urgent), or a dead server would read
// as an empty inbox.

BarWidget {
  id: root
  moduleName: "pneu"

  readonly property int staleAfter: Status.STALE_AFTER

  property var doc: null    // parsed status.json (Status.parse), or null
  property bool missing: true
  property double now: Date.now()
  // When this widget last ran Reset window data: Reopen pneu shows in the
  // menu for a while after. The widget's own state, not the file's.
  property double resetAt: 0

  readonly property var model: Status.model(doc, missing, now)
  readonly property bool warning: model.warning
  readonly property int unread: model.unread
  readonly property var pullingAccounts: model.pulling
  readonly property int pullPercent: model.pullPercent
  readonly property bool resetPending: resetAt > 0 && now - resetAt < Status.REOPEN_FOR
  readonly property var menuItems: Status.menu(model, resetPending)

  readonly property bool minimal: String(setting("style", "Default")) === "Minimal"

  // `shown`, not `visible`, is what a host that mounts the widget itself
  // should bind: `visible` reads false while any parent is hidden, so a
  // mount that hid on it could never come back.
  readonly property bool shown: !minimal || warning || unread > 0
  visible: shown
  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

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
    root.doc = Status.parse(raw)
    root.missing = missing
    root.now = Date.now()
  }

  // Staleness is a function of the clock, not of the file, so the clock is
  // what ticks. A minute is fine resolution against a 20-minute threshold;
  // a client's connecting grace is 90s, so it rereads sooner while one is
  // connecting.
  Timer {
    interval: root.model.connecting ? 15000 : 60000
    running: true
    repeat: true
    onTriggered: root.now = Date.now()
  }

  readonly property var fmt: ({
    clock: function (ms) { return Qt.formatDateTime(new Date(ms), "HH:mm") },
    day: function (ms) { return Qt.formatDateTime(new Date(ms), "d MMM yyyy") }
  })

  readonly property string tooltip: Status.tooltip(model, fmt)

  function open() {
    if (!root.bar) return
    var command = String(root.setting("command", "")).trim()
    // bar.run is `bash -lc`, and Omarchy's login profile puts ~/.local/bin
    // on PATH, so the bare name finds a ~/.local/bin install.
    root.bar.run(command !== "" ? command : "pneu open")
  }

  // ---- the menu (docs/client.md, "The action menu"; R1) ----
  // Launchers live here and nowhere else: a page can't reach this widget
  // or the control socket the commands talk to.

  property bool confirming: false

  function toggleMenu() {
    root.confirming = false
    menu.open = !menu.open
  }

  // act runs a menu item: Open as a click does; Reset window data asks
  // first (it closes the windows and deletes saved drafts); everything else
  // is the item's fixed argv from status.js, without a shell.
  function act(id) {
    if (id === "cancel") { root.confirming = false; return }
    if (id === "open") { menu.open = false; root.open(); return }
    if (id === "reset" && !root.confirming) { root.confirming = true; return }
    if (Status.argv(id) === null) return
    menu.open = false
    root.confirming = false
    if (id === "reset") { root.resetAt = Date.now(); root.now = root.resetAt }
    if (id === "reopen") root.resetAt = 0
    Util.execArgv(Status.argv(id))
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
      ? (root.bar ? root.bar.urgent : Commons.Color.urgent)
      : (root.unread > 0 ? Commons.Color.accent : (root.bar ? root.bar.barForeground : Commons.Color.foreground))
    // A first download shows its progress in place of the count.
    readonly property bool pulling: !root.warning && root.pullingAccounts.length > 0
    readonly property bool showCount: !root.minimal && (pulling || root.unread > 0)
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
    onPressed: function (b) {
      if (b === Qt.LeftButton) root.open()
      else if (b === Qt.RightButton) root.toggleMenu()
    }

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

  // The menu card. Its labels are status.js's fixed strings, drawn by the
  // shell's Button (PlainText); outside clicks close it (PopupCard's focus
  // grab), as does another popup opening.
  PopupCard {
    id: menu
    anchorItem: button
    bar: root.bar
    contentWidth: menu.fittedContentWidth(Style.space(280))
    contentHeight: menu.fittedContentHeight(menuColumn.implicitHeight)
    onOpenChanged: if (!open) root.confirming = false

    Column {
      id: menuColumn
      width: parent.width
      spacing: Style.space(2)

      Text {
        visible: root.confirming
        width: parent.width
        wrapMode: Text.WordWrap
        text: Status.RESET_CONFIRM
        textFormat: Text.PlainText
        color: Commons.Color.foreground
        font.family: Style.font.family
        font.pixelSize: Style.font.body
      }

      Repeater {
        model: root.confirming
          ? [{ id: "reset", label: "Reset window data" }, { id: "cancel", label: "Cancel" }]
          : root.menuItems
        delegate: Button {
          required property var modelData
          width: menuColumn.width
          leftAlign: true
          text: modelData.label
          onClicked: root.act(modelData.id)
        }
      }
    }
  }
}
