// ConnPicker.qml — SPC C: which connection this query runs on.
//
// Opens on the active connection (App.activeConnection); Enter makes a row the
// query's and closes it; Esc leaves it as it was.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "connection for this query"
    dim: false
    standardButtons: Dialog.Cancel
    ListView {
        palette.highlight: Theme.document.highlight
        palette.highlightedText: Theme.document.highlightedText
        model: App.connections
        textRole: "label"
        currentIndex: App.activeConnection
        onActivated: App.chooseConnection(index)
    }
}
