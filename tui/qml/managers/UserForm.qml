// One form for a new user, role, passphrase reset, or connection grant.
Dialog {
    closeOnQ: true
    maxWidthPercent: 80
    maxHeightPercent: 80
    title: App.userFormTitle
    width: 72
    dim: false
    standardButtons: Dialog.Ok | Dialog.Cancel
    defaultButton: Dialog.Ok
    helpText: App.userFormError
    onOpened: { name.clear(); passphrase.clear() }
    onRejected: { passphrase.clear(); App.userFormCancelled() }
    Flex {
        direction: Tui.Vertical
        Text { text: qsTrId("autodb.userForm.text"); visible: App.userFormNaming }
        TextField { id: name; visible: App.userFormNaming }
        Text { text: qsTrId("autodb.userForm.text2"); visible: App.userFormRoling }
        ComboBox { id: role; visible: App.userFormRoling; model: App.roles; textRole: "label"; valueRole: "id"; currentIndex: App.userFormRoleIndex }
        Text { text: qsTrId("autodb.userForm.text3"); visible: App.userFormGranting }
        ComboBox { id: conn; visible: App.userFormGranting; model: App.userConnections; textRole: "label"; valueRole: "id"; currentIndex: App.userFormConnIndex }
        Text { text: qsTrId("autodb.userForm.text4"); visible: App.userFormPassphrase }
        TextField { id: passphrase; visible: App.userFormPassphrase; echoMode: TextInput.Password }
    }
    onAccepted: { App.saveUser(name.text, role.currentValue, conn.currentValue, passphrase.text); passphrase.clear() }
}
