import QtQuick
Item {
  property QtObject bar: null
  property string moduleName: ""
  property var settings: ({})
  readonly property bool vertical: false
  function setting(n, f) { var v = settings ? settings[n] : undefined; return v === undefined || v === null ? f : v }
}
