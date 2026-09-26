// Consent and its consequence share one scrollable dialog, not stacked cards.
Dialog {
    title: App.keyslotConfirmTitle
    dim: false
    onRejected: App.keyslotConfirmCancelled()
    Editor { readOnly: true; text: App.keyslotQuestion }
    DialogButtonBox {
        Button { text: "&Proceed"; DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole }
        Button { text: "&Cancel"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
    onAccepted: App.keyslotConfirmed()
}
