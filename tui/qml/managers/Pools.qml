// Pools.qml — the connection pools and their live budgets (admin).
//
// Each share reads held/limit: the front door's (sessions like a client's) and
// the headroom (the TUI, exec and autodb's own queries). New asks a name and a
// max; Set max asks only the max, for the selected pool.
Dialog {
    closeOnQ: true
    maxWidthPercent: 90
    maxHeightPercent: 80
    title: qsTrId("autodb.pools.title")
    dim: false
    helpText: App.poolsStatus
    onRejected: App.poolsClosed()
    Flex {
        direction: Tui.Vertical
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: table
            model: App.poolRows
            Layout.fillHeight: true
            currentIndex: App.poolIndex
            TableViewColumn { role: "name"; title: "POOL"; width: 22 }
            TableViewColumn { role: "max"; title: "MAX"; width: 6 }
            TableViewColumn { role: "frontDoor"; title: "FRONT DOOR"; width: 12 }
            TableViewColumn { role: "headroom"; title: "HEADROOM"; width: 10 }
            TableViewColumn { role: "waiting"; title: "WAITING"; width: 9 }
            TableViewColumn { role: "connections"; title: "CONNECTIONS"; width: 0 }
        }
        Flex {
            direction: Tui.Horizontal
            visible: App.poolEditing
            TextField { id: name; visible: App.poolFormNew; Layout.fillWidth: true; placeholderText: qsTrId("autodb.pools.placeholderName") }
            TextField { id: max; Layout.fillWidth: true; placeholderText: qsTrId("autodb.pools.placeholderMax"); onAccepted: App.poolSave(name.text, max.text) }
            Button { text: qsTrId("autodb.pools.save"); onClicked: App.poolSave(name.text, max.text) }
        }
    }
    DialogButtonBox {
        Button { text: qsTrId("autodb.pools.new"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole
                 onClicked: { name.clear(); max.clear(); App.poolAdd(); name.forceActiveFocus() } }
        Button { text: qsTrId("autodb.pools.setMax"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole
                 onClicked: { max.clear(); App.poolEdit(table.currentIndex); max.forceActiveFocus() } }
        Button { text: qsTrId("autodb.pools.delete"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.poolDelete(table.currentIndex) }
        Button { text: qsTrId("autodb.pools.close"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
