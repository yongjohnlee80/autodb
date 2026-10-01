// KeyPassphrase.qml — the SSH key file's own passphrase, asked while a
// connect is opening it. It opens the key for this connect (reconnects reuse
// it) and is kept nowhere.

Dialog {
    closeOnQ: false
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: "the SSH key's passphrase"
    width: 80
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    dim: true
    helpText: "not your autodb passphrase: the one that protects this key file"
    Flex {
        direction: Tui.Vertical
        Text { text: App.keyPassphrasePath }
        TextField { id: keyPass; echoMode: TextInput.Password }
    }
    onOpened: keyPass.clear()
    onAccepted: { App.keyPassphraseAnswered(keyPass.text); keyPass.clear() }
    onRejected: { keyPass.clear(); App.keyPassphraseCancelled() }
}
