// Show-once token and the live listener details a client needs to use it.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.cardTitle
    dim: false
    helpText: "Select and yank with v/y · Copy all button · q/Esc close"
    onRejected: App.cardClosed()
    Editor { palette.highlight: Theme.document.highlight; palette.highlightedText: Theme.document.highlightedText; readOnly: true; text: App.cardText }
    DialogButtonBox {
        Button { text: "Cop&y all"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.copyCard() }
        Button { text: "Close(&q)"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
