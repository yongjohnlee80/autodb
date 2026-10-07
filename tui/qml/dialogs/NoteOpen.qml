// File → Open note: a literal name/workspace filter over this account's local notes.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.noteOpen.title")
    dim: false
    standardButtons: Dialog.Close
    helpText: App.noteOpenStatus
    onOpened: filter.clear()
    onRejected: App.noteOpenClosed()
    Flex {
        direction: Tui.Vertical
        TextField { id: filter; placeholderText: qsTrId("autodb.noteOpen.placeholderText"); onTextEdited: App.noteOpenFilter(filter.text) }
        TableView {
            palette.highlight: Theme.document.highlight
            palette.highlightedText: Theme.document.highlightedText
            model: App.noteOpenRows
            Layout.fillHeight: true
            onActivated: App.noteOpenSelect(index)
            TableViewColumn { role: "workspace"; title: "WORKSPACE"; width: 24 }
            TableViewColumn { role: "name"; title: "NOTE"; width: 0 }
        }
    }
}
