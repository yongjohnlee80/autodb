// ConfirmQuit.qml — q asks; Ctrl+Q does not.

Dialog {
    title: "quit"
    standardButtons: Dialog.Yes | Dialog.No
    dim: true
    onAccepted: App.quitConfirmed()
    Text { text: App.quitQuestion }
}
