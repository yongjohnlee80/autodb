// Show-once token and the live listener details a client needs to use it.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.cardTitle
    dim: false
    helpText: qsTrId("autodb.connCard.helpText")
    onRejected: App.cardClosed()
    Editor { palette.highlight: Theme.document.highlight; palette.highlightedText: Theme.document.highlightedText; readOnly: true; text: App.cardText }
    DialogButtonBox {
        Button { text: qsTrId("autodb.connCard.text"); DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.copyCard() }
        Button { text: qsTrId("autodb.connCard.text2"); DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
