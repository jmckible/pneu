pragma Singleton
import QtQuick
QtObject {
  function space(n) { return n }
  property var font: ({ caption: 10, body: 12, family: "monospace" })
  property var bar: ({ iconFont: 14 })
}
