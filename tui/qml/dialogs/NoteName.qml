// NoteName.qml — name a note: New note, and Save note as.

Dialog {
    title: App.noteNameTitle
    standardButtons: Dialog.Ok | Dialog.Cancel
    helpText: App.noteNameError
    Flex {
        direction: Tui.Vertical
        Text { text: "workspace" }
        ComboBox { id: workspace; model: App.workspaces; textRole: "name"; valueRole: "id" }
        Text { text: "name" }
        TextField { id: name; placeholderText: "report.sql" }
    }
    onAccepted: App.nameNote(workspace.currentValue, name.text)
}
