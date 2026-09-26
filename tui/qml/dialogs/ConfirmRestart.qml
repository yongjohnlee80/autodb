// Only a terminal frontend with a spawner offers this admin action.
Dialog {
    title: "restart server"
    dim: false
    standardButtons: Dialog.Yes | Dialog.No
    Text { wrapMode: Tui.WordWrap; text: App.restartQuestion }
    onAccepted: App.restartConfirmed()
}
