// One view for a user's own IPs and the admin global allowlist.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.addressesTitle
    dim: false
    helpText: App.addressesStatus
    onRejected: App.addressesClosed()
    Flex {
        direction: Tui.Vertical
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            id: table
            model: App.addressRows
            Layout.fillHeight: true
            currentIndex: App.addressIndex
            TableViewColumn { role: "source"; title: "SOURCE"; width: 10 }
            TableViewColumn { role: "cidr"; title: "CIDR"; width: 23 }
            TableViewColumn { role: "note"; title: "NOTE"; width: 0 }
        }
        Flex {
            direction: Tui.Horizontal
            visible: App.addressAdding
            TextField { id: cidr; Layout.fillWidth: true; placeholderText: qsTrId("autodb.addresses.placeholderText") }
            TextField { id: note; Layout.fillWidth: true; placeholderText: qsTrId("autodb.addresses.placeholderText2"); onAccepted: App.addressSave(cidr.text, note.text) }
            Button { text: qsTrId("autodb.addresses.text"); onClicked: App.addressSave(cidr.text, note.text) }
        }
    }
    DialogButtonBox {
        Button { text: qsTrId("autodb.addresses.text2"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole
                 onClicked: { cidr.clear(); note.clear(); App.addressAdd(); cidr.forceActiveFocus() } }
        Button { text: qsTrId("autodb.addresses.text3"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.addressRemove(table.currentIndex) }
        Button { text: qsTrId("autodb.addresses.text4"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
