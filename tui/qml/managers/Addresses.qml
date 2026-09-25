// Addresses.qml — IP allowlists: the global one (admin), and each user's own.
// One screen for both; the host says whose list it is.

Dialog {
    title: App.addressesTitle
    standardButtons: Dialog.Close
    helpText: App.addressesStatus
    Flex {
        direction: Tui.Vertical
        TableView {
            id: table
            model: App.addressRows
            TableViewColumn { role: "source"; title: "SOURCE"; width: 10 }
            TableViewColumn { role: "cidr";   title: "CIDR";   width: 20 }
            TableViewColumn { role: "note";   title: "NOTE";   width: 0 }
        }
        Flex {
            direction: Tui.Horizontal
            Button { label: "&Add";    onClicked: App.addressAdd() }
            Button { label: "&Remove"; onClicked: App.addressRemove(table.currentIndex) }
        }
        Flex {
            direction: Tui.Horizontal
            visible: App.addressAdding
            TextField { id: cidr; placeholderText: "203.0.113.4/32" }
            TextField { id: note; placeholderText: "label" }
            Button { label: "&Save"; onClicked: App.addressSave(cidr.text, note.text) }
        }
    }
}
