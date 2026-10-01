// The QML engine loads shell/status.js as BarWidget.qml does, and agrees
// with node (shell/status.test.js has the full cases):
//   QT_QPA_PLATFORM=offscreen /usr/lib/qt6/bin/qmltestrunner -input shell/test
import QtQuick
import QtTest
import "../status.js" as Status

TestCase {
  name: "status"
  function test_v2() {
    var now = Date.parse("2026-09-30T12:00:00Z")
    var raw = JSON.stringify({version: 2, updated: "2026-09-30T11:59:00Z", running: true, unread: 3, senders: ["A‮B"], accounts: [],
      server: {name: "server", link: "down", linkSince: "2026-09-30T11:50:00Z", statusAt: "2026-09-30T11:40:00Z", reason: "node-offline", update: null}})
    var m = Status.model(Status.parse(raw), false, now)
    compare(m.warning, true)
    var fmt = { clock: function (ms) { return Qt.formatDateTime(new Date(ms), "HH:mm") }, day: function (ms) { return "" } }
    var tip = Status.tooltip(m, fmt)
    verify(tip.indexOf("Can't reach server") >= 0, tip)
    verify(tip.indexOf("AB") >= 0, tip)
    compare(Status.menu(m, false).map(function (i) { return i.id }).join(","), "open,agent,reset")
    compare(Status.argv("agent").join(" "), "pneu agent")
    compare(Status.clean("x y"), "xy")
  }
  function test_update() {
    var raw = JSON.stringify({version: 2, updated: "2026-09-30T11:59:00Z", running: true, unread: 0, senders: [], accounts: [],
      server: {name: "server", link: "up", linkSince: "2026-09-30T11:50:00Z", statusAt: "2026-09-30T11:59:00Z", reason: null,
        update: {state: "different", client: "unknown", server: "unknown"}}})
    var m = Status.model(Status.parse(raw), false, Date.parse("2026-09-30T12:00:00Z"))
    compare(Status.menu(m, false).map(function (i) { return i.id }).join(","), "open,update,reset")
    compare(Status.argv("update").join(" "), "omarchy-launch-floating-terminal-with-presentation pneu update")
  }
  function test_v1() {
    var m = Status.model(Status.parse('{"version":1,"updated":"2026-09-30T11:59:00Z","running":true,"unread":0,"senders":[],"accounts":[]}'), false, Date.parse("2026-09-30T12:00:00Z"))
    compare(m.warning, false)
  }
}
