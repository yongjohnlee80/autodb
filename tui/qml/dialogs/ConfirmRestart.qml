// Only a terminal frontend with a spawner offers this admin action. No takes
// initial focus, so Enter cannot restart the server by accident.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.confirmRestart.title")
    dim: false
    Text { wrapMode: Tui.WordWrap; text: App.restartQuestion }
    DialogButtonBox {
        Button { text: qsTrId("autodb.confirmRestart.text"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
        Button { text: qsTrId("autodb.confirmRestart.text2"); DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole }
    }
    onAccepted: App.restartConfirmed()
}
