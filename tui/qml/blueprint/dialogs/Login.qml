// Login.qml — sign in: a user and a passphrase. Enter moves to the next field
// and, on the last, signs in.

Dialog {
    title: "sign in"
    standardButtons: Dialog.Ok | Dialog.Cancel
    dim: true
    helpText: App.loginError
    Flex {
        direction: Tui.Vertical
        Text { text: "user" }
        TextField { id: user; text: App.lastUser }
        Text { text: "passphrase" }
        TextField { id: passphrase; echoMode: TextInput.Password }
    }
    onAccepted: App.login(user.text, passphrase.text)
}
