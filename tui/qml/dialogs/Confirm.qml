// Confirm.qml — a question before an action: delete this, open the front door
// on that. Its title, text and answers are the question's (App.confirm*).
//
// A DialogButtonBox: no default, so a stray Enter answers nothing; the action
// is its mnemonic, Space on it, or a click. Esc, or the other answer, leaves
// things as they are.

Dialog {
    title: App.confirmTitle
    dim: true
    Text { wrapMode: Tui.WordWrap; text: App.confirmText }
    DialogButtonBox {
        Button { text: App.confirmYes; DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole; onClicked: App.confirmed("yes") }
        Button { text: App.confirmNo;  DialogButtonBox.buttonRole: DialogButtonBox.RejectRole; onClicked: App.confirmed("no") }
    }
}
