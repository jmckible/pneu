pragma Singleton
import QtQuick
QtObject {
  property var calls: []
  function execArgv(a) { calls.push(a) }
  function execDetached(c) { calls.push(["SHELL", c]) }
}
