// Login.qml — sign in: a user and a passphrase.
//
// Answering closes it, as a Qt dialog closes; a refused answer — no user, or
// the server saying no — opens it again with the reason on its help line
// (App.loginError). The user field keeps the name last tried; the passphrase
// starts empty each time.

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: qsTrId("autodb.login.title")
    width: 72
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok          // Enter signs in, from the last field too
    dim: true
    helpText: App.loginError
    Flex {
        direction: Tui.Vertical
        Text { text: qsTrId("autodb.login.text") }
        TextField { id: user; text: App.lastUser }
        Text { text: qsTrId("autodb.login.text2") }
        TextField { id: passphrase; echoMode: TextInput.Password }
    }
    onOpened: passphrase.clear()
    onAccepted: App.login(user.text, passphrase.text)
    onRejected: App.signInDeclined()
}
