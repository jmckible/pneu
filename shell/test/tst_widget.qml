// The real BarWidget.qml under Qt 6, against stand-ins for the shell it
// runs in (imports/: qs.Commons, qs.Ui, Quickshell; only what the widget
// touches). It loads, draws a v2 file, and its menu runs only the fixed
// argv, through Util.execArgv:
//   QT_QPA_PLATFORM=offscreen /usr/lib/qt6/bin/qmltestrunner -import shell/test/imports -input shell/test
import QtQuick
import QtTest
import qs.Commons
import ".." as Pneu

TestCase {
  name: "widget"
  Pneu.BarWidget { id: w }
  QtObject { id: fakeBar; property color urgent: "red"; property color barForeground: "white"; property var ran: []; function run(c) { ran.push(c) } }
  function test_flow() {
    w.bar = fakeBar
    var now = new Date().toISOString()
    w.apply(JSON.stringify({version: 2, updated: now, running: true, unread: 2, senders: ["<b>A</b>"], accounts: [],
      server: {name: "server", link: "down", linkSince: now, statusAt: now, reason: "refused", update: null}}), false)
    compare(w.warning, true)
    verify(w.tooltip.indexOf("Can't reach server") === 0, w.tooltip)
    compare(w.menuItems.map(function (i) { return i.id }).join(","), "open,agent,reset")
    w.toggleMenu()
    w.act("agent")
    compare(JSON.stringify(Util.calls[Util.calls.length - 1]), '["pneu","agent"]')
    w.act("reset")
    compare(w.confirming, true)
    w.act("cancel")
    compare(w.confirming, false)
    w.act("reset"); w.act("reset")
    compare(JSON.stringify(Util.calls[Util.calls.length - 1]), '["pneu","reset-window"]')
    compare(w.menuItems.map(function (i) { return i.id }).join(","), "open,agent,reopen,reset")
    w.act("open")
    compare(fakeBar.ran[fakeBar.ran.length - 1], "pneu open")
    w.act("reopen")
    compare(JSON.stringify(Util.calls[Util.calls.length - 1]), '["pneu","open"]')
    compare(w.resetPending, false)
    w.apply('{"version":1,"updated":"' + now + '","running":true,"unread":0,"senders":[],"accounts":[]}', false)
    compare(w.warning, false)
    compare(w.tooltip, "No unread mail")
    w.apply("", true)
    verify(w.tooltip.indexOf("no status yet") >= 0)
  }
  // The version nudge: the update items by server.update, each running its
  // fixed argv.
  function test_update() {
    w.bar = fakeBar
    var now = new Date().toISOString()
    var doc = function (state) {
      return JSON.stringify({version: 2, updated: now, running: true, unread: 0, senders: [], accounts: [],
        server: {name: "server", link: "up", linkSince: now, statusAt: now, reason: null,
          update: {state: state, client: "$(id)", server: "<b>x</b>"}}})
    }
    w.apply(doc("server-older"), false)
    compare(w.menuItems.map(function (i) { return i.id }).join(","), "open,updateServer,reset")
    compare(w.menuItems[1].label, "Update server")
    w.act("updateServer")
    compare(JSON.stringify(Util.calls[Util.calls.length - 1]), '["pneu","agent","-update"]')
    w.apply(doc("client-older"), false)
    compare(w.menuItems.map(function (i) { return i.id }).join(","), "open,update,reset")
    w.act("update")
    compare(JSON.stringify(Util.calls[Util.calls.length - 1]), '["omarchy-launch-floating-terminal-with-presentation","pneu","update"]')
    verify(w.tooltip.indexOf("right-click: Update pneu") >= 0, w.tooltip)
    w.apply(doc("__proto__"), false)
    compare(w.menuItems.map(function (i) { return i.id }).join(","), "open,reset")
  }
}
