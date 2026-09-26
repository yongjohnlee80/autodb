// Bootstrap.qml — the server has no users yet: name its root user.
//
// The root user is "root" unless named, and its passphrase also unlocks the
// server's master key, so it is asked twice. A refused answer opens it again
// with the reason (App.bootstrapError).

Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "first run — create the root user"
    width: 72
    dim: false        // the first thing an operator sees: nothing behind it to fade
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok          // Enter creates the user, from the last field too
    helpText: App.bootstrapError
    Flex {
        direction: Tui.Vertical
        Text { text: "root user (root if empty)" }
        TextField { id: user }
        Text { text: "passphrase — at least 8 characters" }
        TextField { id: passphrase; echoMode: TextInput.Password }
        Text { text: "again" }
        TextField { id: again; echoMode: TextInput.Password }
    }
    onOpened: {
        passphrase.clear()
        again.clear()
    }
    onAccepted: App.bootstrap(user.text, passphrase.text, again.text)
    onRejected: App.signInDeclined()
}
