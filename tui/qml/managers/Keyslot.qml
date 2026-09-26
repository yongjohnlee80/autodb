// Admin: distinguish the boot probe from what has been verified since start.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "service keyslot"
    dim: false
    helpText: App.keyslotStatus
    onRejected: App.keyslotClosed()
    Text { wrapMode: Tui.WordWrap; text: App.keyslotText }
    DialogButtonBox {
        Button { text: "&Enable"; enabled: App.keyslotCanEnable; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.keyslotEnable() }
        Button { text: "&Remove"; enabled: App.keyslotCanRemove; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.keyslotRemove() }
        Button { text: "Close(&q)"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
