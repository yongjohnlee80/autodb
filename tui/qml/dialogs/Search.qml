// Literal row search in the pane that had focus when / was pressed.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.searchTitle
    dim: false
    width: 48
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.searchError
    TextField { id: pattern; text: App.lastSearch; placeholderText: qsTrId("autodb.search.placeholderText") }
    onAccepted: App.search(pattern.text)
    onRejected: App.searchCancelled()
}
