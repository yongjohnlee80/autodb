// File → Open note: a literal name/workspace filter over this account's local notes.
Dialog {
    title: "open a note"
    dim: false
    standardButtons: Dialog.Close
    helpText: App.noteOpenStatus
    onOpened: filter.clear()
    onRejected: App.noteOpenClosed()
    Flex {
        direction: Tui.Vertical
        TextField { id: filter; placeholderText: "filter by note or workspace"; onTextEdited: App.noteOpenFilter(filter.text) }
        TableView {
            model: App.noteOpenRows
            Layout.fillHeight: true
            onActivated: App.noteOpenSelect(index)
            TableViewColumn { role: "workspace"; title: "WORKSPACE"; width: 24 }
            TableViewColumn { role: "name"; title: "NOTE"; width: 0 }
        }
    }
}
