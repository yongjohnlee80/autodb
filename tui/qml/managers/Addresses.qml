// One view for a user's own IPs and the admin global allowlist.
Dialog {
    title: App.addressesTitle
    dim: false
    helpText: App.addressesStatus
    onRejected: App.addressesClosed()
    Flex {
        direction: Tui.Vertical
        TableView {
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
            TextField { id: cidr; Layout.fillWidth: true; placeholderText: "IP or CIDR (blank = this address for personal list)" }
            TextField { id: note; Layout.fillWidth: true; placeholderText: "label"; onAccepted: App.addressSave(cidr.text, note.text) }
            Button { text: "&Save"; onClicked: App.addressSave(cidr.text, note.text) }
        }
    }
    DialogButtonBox {
        Button { text: "&Add"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole
                 onClicked: { cidr.clear(); note.clear(); App.addressAdd(); cidr.forceActiveFocus() } }
        Button { text: "&Remove"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.addressRemove(table.currentIndex) }
        Button { text: "&Close"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
