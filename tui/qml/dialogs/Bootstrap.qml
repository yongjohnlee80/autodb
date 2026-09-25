// Bootstrap.qml — the server has no users: name the root user.

Dialog {
    title: "create the root user"
    standardButtons: Dialog.Ok | Dialog.Cancel
    dim: true
    helpText: App.bootstrapError
    Flex {
        direction: Tui.Vertical
        Text { text: "root user" }
        TextField { id: user }
        Text { text: "passphrase" }
        TextField { id: passphrase; echoMode: TextInput.Password }
        Text { text: "again" }
        TextField { id: again; echoMode: TextInput.Password }
    }
    onAccepted: App.bootstrap(user.text, passphrase.text, again.text)
}
