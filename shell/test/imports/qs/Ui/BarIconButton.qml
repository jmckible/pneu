import QtQuick
Item { property QtObject bar; property Component iconComponent; property real slotSize; property real opticalSize; property string tooltipText; property string fontFamily: "monospace"; signal pressed(int button)
  Loader { sourceComponent: parent.iconComponent } }
