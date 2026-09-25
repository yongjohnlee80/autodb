// ConfirmQuit.qml — quitting asks first, whichever way it was asked for: `q`,
// Ctrl+Q, SPC Q or Home › Exit. A stray key must not end the session.
//
// The answers NAME BOTH OUTCOMES — quit or stay, not a yes to a question —
// under the hand that just pressed Ctrl+Q by accident. Neither is a default:
// Enter answers nothing (a DialogButtonBox declares none), and `q` is not a
// mnemonic here, so a double-tapped `q` is a safe no-op.

Dialog {
    title: "quit autodb?"
    dim: true
    onAccepted: App.quitConfirmed()
    Text { text: App.quitQuestion }
    DialogButtonBox {
        Button { text: "&Yes, quit";  DialogButtonBox.buttonRole: DialogButtonBox.AcceptRole }
        Button { text: "&No, stay";   DialogButtonBox.buttonRole: DialogButtonBox.RejectRole }
    }
}
