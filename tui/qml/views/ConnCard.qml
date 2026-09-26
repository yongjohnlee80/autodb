// Show-once token and the live listener details a client needs to use it.
Dialog {
    title: App.cardTitle
    dim: false
    helpText: "Select and yank with v/y · Copy all button · Esc close"
    onRejected: App.cardClosed()
    Editor { readOnly: true; text: App.cardText }
    DialogButtonBox {
        Button { text: "Cop&y all"; DialogButtonBox.buttonRole: DialogButtonBox.ActionRole; onClicked: App.copyCard() }
        Button { text: "&Close"; DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
