// Literal row search in the pane that had focus when / was pressed.
Dialog {
    title: App.searchTitle
    dim: false
    width: 48
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.searchError
    TextField { id: pattern; text: App.lastSearch; placeholderText: "find text" }
    onAccepted: App.search(pattern.text)
    onRejected: App.searchCancelled()
}
