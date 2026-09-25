// Login.qml — sign in: a user and a passphrase.
//
// Answering closes it, as a Qt dialog closes; a refused answer — no user, or
// the server saying no — opens it again with the reason on its help line
// (App.loginError). The user field keeps the name last tried; the passphrase
// starts empty each time.

Dialog {
    title: "sign in"
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok          // Enter signs in, from the last field too
    dim: true
    helpText: App.loginError
    Flex {
        direction: Tui.Vertical
        Text { text: "user" }
        TextField { id: user; text: App.lastUser }
        Text { text: "passphrase" }
        TextField { id: passphrase; echoMode: TextInput.Password }
    }
    onOpened: passphrase.clear()
    onAccepted: App.login(user.text, passphrase.text)
    onRejected: App.signInDeclined()
}
