// Only connections not already in this workspace are offered.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
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
