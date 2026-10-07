// Create or rename a workspace. A failed answer reopens with its reason.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.workspaceFormTitle
    width: 72
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.workspaceFormError
    onRejected: App.workspaceNameCancelled()
    Flex {
        direction: Tui.Vertical
        Text { text: qsTrId("autodb.workspaceName.text") }
        TextField { id: name; text: App.workspaceFormName }
    }
    onAccepted: App.saveWorkspace(name.text)
}
