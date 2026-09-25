// Inspect.qml — one result row, a column per line: `name = value`.
//
// y copies the value under the cursor exactly; Enter opens it whole.

Dialog {
    title: "row"
    standardButtons: Dialog.Close
    helpText: "j/k move · y copy · Enter open"
    ListView {
        model: App.inspectRows
        textRole: "line"
        onActivated: App.openValue(index)
    }
    Shortcut { sequence: "y"; onActivated: App.copyInspected() }
}
