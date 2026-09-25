// Search.qml — `/`: find in the pane in use (the query, the results); n/N step.

Dialog {
    title: "find"
    standardButtons: Dialog.Ok | Dialog.Cancel
    width: 48
    TextField { id: pattern; text: App.lastSearch }
    onAccepted: App.search(pattern.text)
}
