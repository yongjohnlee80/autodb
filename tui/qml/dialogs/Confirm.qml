// Confirm.qml — a question before an action: delete this, open the front door
// on that. Its title, text and answers are the question's (App.confirm*).
//
// The harmless No answer takes initial focus: Enter activates it, while the
// action requires its mnemonic, moving focus, or a click. Esc also leaves
// things as they are.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.confirmTitle
    dim: true
    Text { wrapMode: Tui.WordWrap; text: App.confirmText }
    DialogButtonBox {
        Button { text: App.confirmNo;  DialogButtonBox.buttonRole: DialogButtonBox.RejectRole; onClicked: App.confirmed("no") }
        Button { text: App.confirmYes; DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole; onClicked: App.confirmed("yes") }
    }
}
