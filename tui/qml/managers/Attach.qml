// Attach.qml — put a connection in a workspace: one it is not in yet.

Dialog {
    title: App.attachTitle
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.attachError
    ComboBox { id: workspace; model: App.attachWorkspaces; textRole: "name"; valueRole: "id"; currentIndex: App.attachWorkspace }
    onAccepted: App.attachConnection(workspace.currentValue, "")
}
