import QtQuick
Item {
  property Item anchorItem
  property QtObject bar
  property bool open: false
  property int contentWidth
  property int contentHeight
  function fittedContentWidth(w) { return w }
  function fittedContentHeight(h) { return h + 10 }
}
