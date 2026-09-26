// Create or rename a workspace. A failed answer reopens with its reason.
Dialog {
    title: App.workspaceFormTitle
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.workspaceFormError
    onRejected: App.workspaceNameCancelled()
    Flex {
        direction: Tui.Vertical
        Text { text: "name" }
        TextField { id: name; text: App.workspaceFormName }
    }
    onAccepted: App.saveWorkspace(name.text)
}
