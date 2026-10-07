// The public CA document itself, or an explicit system-roots explanation.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.caTitle
    dim: false
    helpText: App.caStatus
    onRejected: App.caClosed()
    Editor { palette.highlight: Theme.document.highlight; palette.highlightedText: Theme.document.highlightedText; readOnly: true; text: App.caText }
    DialogButtonBox {
        Button { text: qsTrId("autodb.cacert.text"); enabled: App.caCanCopy; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.copyCA() }
        Button { text: qsTrId("autodb.cacert.text2"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
