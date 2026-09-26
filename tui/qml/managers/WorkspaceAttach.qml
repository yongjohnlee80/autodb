// Only connections not already in this workspace are offered.
Dialog {
    title: App.workspaceAttachTitle
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.workspaceAttachError
    onRejected: App.workspaceAttachCancelled()
    ComboBox {
        id: connection
        model: App.workspaceAttachOptions
        textRole: "name"
        valueRole: "id"
        currentIndex: App.workspaceAttachIndex
    }
    onAccepted: App.attachToWorkspace(connection.currentValue, "")
}
