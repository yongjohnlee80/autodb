// CIDRs added to the account remain after this one token is revoked.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.widening.title")
    dim: false
    Editor { palette.highlight: Theme.document.highlight; palette.highlightedText: Theme.document.highlightedText; readOnly: true; text: App.widenText }
    DialogButtonBox {
        Button { text: qsTrId("autodb.widening.text"); DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole }
        Button { text: qsTrId("autodb.widening.text2"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
    onAccepted: App.widenAccepted()
    onRejected: App.widenCancelled()
}
