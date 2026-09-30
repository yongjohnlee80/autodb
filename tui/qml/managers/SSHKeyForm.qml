// An SSH key on one's own profile: added (the public key and the autodb
// passphrase) or labelled.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.keyFormTitle
    width: 80
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.keyFormError
    onOpened: passphrase.clear()
    onRejected: { passphrase.clear(); App.keyFormClosed() }
    Flex {
        direction: Tui.Vertical
        Text { visible: App.keyFormAdding; text: "public key (the one line of your .pub file)" }
        TextField { id: publicKey; visible: App.keyFormAdding; text: App.keyFormPublic }
        Text { text: "label (which computer or key it is)" }
        TextField { id: label; text: App.keyFormLabel }
        Text { visible: App.keyFormAdding; text: "your autodb passphrase (a wrong one signs you out)" }
        TextField { id: passphrase; visible: App.keyFormAdding; echoMode: TextInput.Password }
    }
    onAccepted: { App.saveKey(publicKey.text, label.text, passphrase.text); passphrase.clear() }
}
