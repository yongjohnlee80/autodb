// Attach.qml — put a connection in a workspace: one it is not in yet.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.attachTitle
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.attachError
    ComboBox { id: workspace; model: App.attachWorkspaces; textRole: "name"; valueRole: "id"; currentIndex: App.attachWorkspace }
    onAccepted: App.attachConnection(workspace.currentValue, "")
}
