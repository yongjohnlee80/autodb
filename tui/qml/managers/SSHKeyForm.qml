// An SSH key on a profile: added (the public key, and the autodb passphrase
// on one's own profile) or labelled.
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
        Text { visible: App.keyFormAdding; text: qsTrId("autodb.sshKeyForm.text") }
        TextField { id: publicKey; visible: App.keyFormAdding; text: App.keyFormPublic }
        Text { text: qsTrId("autodb.sshKeyForm.text2") }
        TextField { id: label; text: App.keyFormLabel }
        Text { visible: App.keyFormPassphrase; text: qsTrId("autodb.sshKeyForm.text3") }
        TextField { id: passphrase; visible: App.keyFormPassphrase; echoMode: TextInput.Password }
    }
    onAccepted: { App.saveKey(publicKey.text, label.text, passphrase.text); passphrase.clear() }
}
