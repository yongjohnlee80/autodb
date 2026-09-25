// ConfirmRestart.qml — restart the server: how many statements it would
// cancel, and that open transactions make it refuse.

Dialog {
    title: "restart server"
    standardButtons: Dialog.Yes | Dialog.No
    onAccepted: App.restartConfirmed()
    Text { wrapMode: Tui.WordWrap; text: App.restartQuestion }
}
