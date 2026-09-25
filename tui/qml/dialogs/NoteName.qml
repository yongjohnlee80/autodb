// NoteName.qml — name a note: New note, and Save note as.
//
// The workspace it goes in, and its name (".sql" is added). A refused name
// opens it again with the reason on its help line (App.noteNameError).

Dialog {
    title: App.noteNameTitle
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok          // Enter names it, from the name field too
    helpText: App.noteNameError
    Flex {
        direction: Tui.Vertical
        Text { text: "workspace" }
        // Starts on the workspace the user is in (App.noteWorkspace).
        ComboBox { id: workspace; model: App.workspaces; textRole: "name"; valueRole: "id"; currentIndex: App.noteWorkspace }
        Text { text: "name" }
        TextField { id: name; placeholderText: "report.sql" }
    }
    onOpened: name.clear()            // a new name each time; the dialog outlives its answers
    onAccepted: App.nameNote(workspace.currentValue, name.text)
}
