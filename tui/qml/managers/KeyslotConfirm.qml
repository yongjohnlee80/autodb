// Consent and its consequence share one scrollable dialog, not stacked cards.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.keyslotConfirmTitle
    dim: false
    onRejected: App.keyslotConfirmCancelled()
    Editor { palette.highlight: Theme.document.highlight; palette.highlightedText: Theme.document.highlightedText; readOnly: true; text: App.keyslotQuestion }
    DialogButtonBox {
        Button { text: "&Proceed"; DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole }
        Button { text: "&Cancel"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
    onAccepted: App.keyslotConfirmed()
}
