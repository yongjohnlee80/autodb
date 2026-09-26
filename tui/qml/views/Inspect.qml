// One result row: full values behind compact one-line column summaries.
Dialog {
    title: "row"
    dim: false
    standardButtons: Dialog.Close
    helpText: "j/k move · y copy · Enter open"
    ListView {
        id: columns
        model: App.inspectRows
        textRole: "line"
        onActivated: App.openValue(index)
    }
    Shortcut { sequence: "y"; onActivated: App.copyInspected(columns.currentIndex) }
}
