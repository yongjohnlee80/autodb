// Admin: distinguish the boot probe from what has been verified since start.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.keyslot.title")
    dim: false
    helpText: App.keyslotStatus
    onRejected: App.keyslotClosed()
    Text { wrapMode: Tui.WordWrap; text: App.keyslotText }
    DialogButtonBox {
        Button { text: qsTrId("autodb.keyslot.text"); enabled: App.keyslotCanEnable; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.keyslotEnable() }
        Button { text: qsTrId("autodb.keyslot.text2"); enabled: App.keyslotCanRemove; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.keyslotRemove() }
        Button { text: qsTrId("autodb.keyslot.text3"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
