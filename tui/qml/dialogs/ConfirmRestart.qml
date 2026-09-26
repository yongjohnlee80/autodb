// Only a terminal frontend with a spawner offers this admin action. No takes
// initial focus, so Enter cannot restart the server by accident.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "restart server"
    dim: false
    Text { wrapMode: Tui.WordWrap; text: App.restartQuestion }
    DialogButtonBox {
        Button { text: "&No, stay"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
        Button { text: "&Yes, restart"; DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole }
    }
    onAccepted: App.restartConfirmed()
}
